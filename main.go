package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blocto/solana-go-sdk/client"
	"github.com/blocto/solana-go-sdk/rpc"
	slogenv "github.com/cbrewster/slog-env"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metric struct {
	totalSlots     uint64
	blocksProduced uint64
}

type clusterMetrics = map[string]metric

type influxConfig struct {
	influxUrl    string
	influxOrg    string
	influxUser   string
	influxPass   string
	influxBucket string
	writeUrl     string
}

func influxConfigFromEnv() influxConfig {
	url := os.Getenv("INFLUX_URL")
	org := os.Getenv("INFLUX_ORG")
	bucket := os.Getenv("INFLUX_BUCKET")

	conf := influxConfig{
		influxUrl:    url,
		influxOrg:    org,
		influxBucket: bucket,
		influxUser:   os.Getenv("INFLUX_USER"),
		influxPass:   os.Getenv("INFLUX_PASSWORD"),
		writeUrl:     fmt.Sprintf("%s/write?org=%s&db=%s", url, org, bucket),
	}

	return conf
}

func (conf influxConfig) enabled() bool {
	return conf.influxUrl != "" &&
		conf.influxOrg != "" &&
		conf.influxUser != "" &&
		conf.influxPass != "" &&
		conf.influxBucket != ""
}

var (
	blocksProduced = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "pyth",
			Name:      "blocks_produced",
			Help:      "Number of blocks produced by a node in a specific epoch",
		},
		[]string{
			"node_id",
			"epoch",
		},
	)

	skippedSlots = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "pyth",
			Name:      "skipped_slots",
			Help:      "Number of slots skipped by a node in a specific epoch",
		},
		[]string{
			"node_id",
			"epoch",
		},
	)
)

func exitIfError(err error, log *slog.Logger) {
	if err != nil {
		log.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func getEnv(varName string, defaultValue string) string {
	value := os.Getenv(varName)
	if value == "" {
		return defaultValue
	}
	return value
}

func epochSlots(c *client.Client, log *slog.Logger, epoch uint64) (uint64, uint64, error) {
	log.Debug("Calling GetEpochSchedule")
	epochScheduleResponse, err := c.RpcClient.GetEpochSchedule(context.TODO())
	if err != nil {
		return 0, 0, err
	}
	if epochScheduleResponse.Error != nil {
		return 0, 0, epochScheduleResponse.GetError()
	}

	schedule := epochScheduleResponse.Result

	// https://github.com/solana-labs/explorer/blob/e2e110654f145893480eab7a7c51f82b50b46d73/app/utils/epoch-schedule.ts#L63
	firstSlot := (epoch-schedule.FirstNormalEpoch)*schedule.SlotsPerEpoch + schedule.FirstNormalSlot
	lastSlot := firstSlot + schedule.SlotsPerEpoch - 1

	slot, err := c.GetSlot(context.TODO())
	if err != nil {
		return 0, 0, err
	}
	if lastSlot > slot {
		lastSlot = slot
	}

	return firstSlot, lastSlot, nil
}

func getBlockProductionForEpoch(c *client.Client, log *slog.Logger, epoch uint64) (clusterMetrics, error) {
	firstSlot, lastSlot, err := epochSlots(c, log, epoch)
	if err != nil {
		return nil, err
	}

	config := rpc.GetBlockProductionConfig{
		Range: &rpc.GetBlockProductionRange{
			FirstSlot: firstSlot,
			LastSlot:  lastSlot,
		},
	}
	log.Debug("Calling GetBlockProductionWithConfig", "config", config)
	blockProductionResponse, err := c.RpcClient.GetBlockProductionWithConfig(context.TODO(), config)
	if err != nil {
		return nil, err
	}
	if blockProductionResponse.Error != nil {
		return nil, blockProductionResponse.GetError()
	}
	log.Debug("Got BlockProduction", "value", blockProductionResponse)

	metrics := make(clusterMetrics)
	for id, slots := range blockProductionResponse.Result.Value.ByIdentity {
		metrics[id] = metric{
			totalSlots:     slots[0],
			blocksProduced: slots[1],
		}
	}

	return metrics, nil
}

func getMetricsForEpoch(c *client.Client, epoch uint64, log *slog.Logger) (clusterMetrics, uint64, error) {
	metrics, err := getBlockProductionForEpoch(c, log, epoch)
	if err != nil {
		log.Error("Error calling getBlockProduction", "error", err, "epoch", epoch)
		return clusterMetrics{}, 0, err
	}

	log.Debug("Calling GetEpochInfo")
	epochInfo, err := c.GetEpochInfo(context.TODO())
	if err != nil {
		log.Error("Error calling GetEpochInfo", "error", err)
		return clusterMetrics{}, 0, err
	}

	return metrics, epochInfo.Epoch, nil
}

func updatePrometheusMetrics(metrics clusterMetrics, epoch uint64) {
	epochStr := fmt.Sprintf("%d", epoch)

	for id, m := range metrics {
		blocksProduced.WithLabelValues(id, epochStr).Set(float64(m.blocksProduced))
		skippedSlots.WithLabelValues(id, epochStr).Set(float64(m.totalSlots - m.blocksProduced))
	}
}

func formatMetricsForInflux(metrics clusterMetrics, epoch uint64) string {
	ret := strings.Builder{}
	for id, m := range metrics {
		ret.WriteString(
			fmt.Sprintf(
				"leader_stats,host_id=%s,epoch=%d skipped_slots=%d,blocks_produced=%d\n",
				id,
				epoch,
				m.totalSlots-m.blocksProduced,
				m.blocksProduced,
			),
		)
	}

	return ret.String()
}

func sendToInflux(metrics clusterMetrics, epoch uint64, conf influxConfig, log *slog.Logger) {
	m := formatMetricsForInflux(metrics, epoch)

	h := &http.Client{}

	rdr := strings.NewReader(m)
	req, err := http.NewRequest("POST", conf.writeUrl, rdr)
	if err != nil {
		log.Error("Error creating InfluxDB POST request", "error", err, "writeUrl", conf.writeUrl, "payload", metrics)
		return
	}
	req.SetBasicAuth(conf.influxUser, conf.influxPass)

	log.Debug("POSTing to InfluxDB")
	resp, err := h.Do(req)
	if err != nil {
		log.Error("Error POSTing to InfluxDB", "error", err, "writeUrl", conf.writeUrl, "user", conf.influxUser, "payload", metrics)
		return
	}

	// If there was an error when posting the data, do not move to the next epoch
	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		exitIfError(err, log)

		log.Error("Error while talking to InfluxDB", "status", resp.StatusCode, "body", body)
		return
	}
}

func verboseMetrics(c *client.Client, epoch uint64, metrics clusterMetrics, log *slog.Logger) {
	startSlot, endSlot, err := epochSlots(c, log, epoch)
	exitIfError(err, log)
	startBlock, err := c.GetBlock(context.TODO(), startSlot)
	startHeight := "Unknown"
	startTime := "Unknown"
	if err != nil {
		log.Warn("Error while getting start slot", "Error", err)
	} else {
		startHeight = fmt.Sprintf("%d", *startBlock.BlockHeight)
		startTime = fmt.Sprintf("%v", *startBlock.BlockTime)
	}
	endBlock, err := c.GetBlock(context.TODO(), endSlot)
	exitIfError(err, log)

	m := formatMetricsForInflux(metrics, epoch)

	fmt.Printf(
		"Current epoch: %d\nStart slot: %v (%v)\nCurrent slot: %v (%v)\n\nMetrics:\n%s\n",
		epoch,
		startHeight,
		startTime,
		*endBlock.BlockHeight,
		*endBlock.BlockTime,
		m,
	)
}

func updateMetrics(c *client.Client, epoch uint64, verbose bool, influxConfig influxConfig, log *slog.Logger) (uint64, error) {
	metrics, newEpoch, err := getMetricsForEpoch(c, epoch, log)
	if err != nil {
		return 0, err
	}

	updatePrometheusMetrics(metrics, epoch)
	if verbose {
		verboseMetrics(c, epoch, metrics, log)
	} else if influxConfig.enabled() {
		sendToInflux(metrics, epoch, influxConfig, log)
	}
	return newEpoch, nil
}

func main() {
	log := slog.New(slogenv.NewHandler(slog.NewJSONHandler(os.Stderr, nil)))

	help := flag.Bool("help", false, "Show help text")
	helpShort := flag.Bool("h", false, "Show help text")
	verbose := flag.Bool("verbose", false, "Run in verbose mode")
	flag.Parse()

	if *help || *helpShort {
		fmt.Fprintln(os.Stderr, `
COMMAND-LINE FLAGS

--help - Show the help text
--verbose - Display the InfluxDB metrics on stdout

ENVIRONMENT VARIABLES

RPC_URL - URL of the PythNet RPC host (optional, default is the PythNet RPC host)
HTTP_PORT - HTTP port for Prometheus metrics (optional, default is 8000)

INFLUX_URL - InfluxDB URL
INFLUX_ORG - InfluxDB organization
INFLUX_USER - InfluxDB user (for token v1 authentication)
INFLUX_PASSWORD - InfluxDB password (for token v1 authentication)
INFLUX_BUCKET - Bucket to save the metrics to`)
		os.Exit(0)
	}

	influxConfig := influxConfigFromEnv()

	rpcUrl := getEnv("RPC_URL", "https://api2.pythnet.pyth.network")

	pollStr := getEnv("POLL_SECONDS", "30")
	pollSecs, err := strconv.Atoi(pollStr)
	exitIfError(err, log)

	httpStr := getEnv("HTTP_PORT", "8000")
	httpPort, err := strconv.Atoi(httpStr)
	exitIfError(err, log)

	log.Info("Serving Prometheus metrics", "port", httpPort, "URI", "/metrics")
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil)
	}()

	c := client.NewClient(rpcUrl)

	epochInfo, err := c.GetEpochInfo(context.TODO())
	exitIfError(err, log)
	currentEpoch := epochInfo.Epoch

	ticker := time.NewTicker(time.Duration(pollSecs) * time.Second)

	// TODO Persist the currentEpoch and publish the metrics for the intervening epochs
	for ; ; <-ticker.C {
		log.Debug("Starting loop")

		newEpoch, err := updateMetrics(c, currentEpoch, *verbose, influxConfig, log)
		if err != nil {
			continue
		}

		if currentEpoch != newEpoch {
			log.Debug("updating currentEpoch", "currentEpoch", currentEpoch)
			currentEpoch = newEpoch
			updateMetrics(c, currentEpoch, *verbose, influxConfig, log)
		}
	}
}
