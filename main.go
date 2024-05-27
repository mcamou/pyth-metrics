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
)

func exitIfError(err error, log *slog.Logger) {
	if err != nil {
		log.Error("Fatal error", "error", err)
		os.Exit(1)
	}
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

func getMetrics(blockProduction map[string][]uint64, epoch uint64) string {
	metrics := strings.Builder{}
	for id, slots := range blockProduction {
		metrics.WriteString(
			fmt.Sprintf(
				"leader_stats,host_id=%s,epoch=%d skipped_slots=%d,blocks_produced=%d\n",
				id,
				epoch,
				slots[0]-slots[1],
				slots[1],
			),
		)
	}

	return metrics.String()
}

func getBlockProductionForEpoch(c *client.Client, log *slog.Logger, epoch uint64) (map[string][]uint64, error) {
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

	return blockProductionResponse.Result.Value.ByIdentity, nil
}

func getEnv(varName string, defaultValue string, log *slog.Logger) string {
	value := os.Getenv(varName)
	if value == "" {
		return defaultValue
	}
	return value
}

func main() {
	log := slog.New(slogenv.NewHandler(slog.NewJSONHandler(os.Stderr, nil)))

	dryRun := false

	help := flag.Bool("help", false, "Show help text")
	helpShort := flag.Bool("h", false, "Show help text")
	flag.Parse()

	if *help || *helpShort {
		fmt.Fprintln(os.Stderr, `
COMMAND-LINE FLAGS

--help - Show the help text

ENVIRONMENT VARIABLES

RPC_URL - URL of the PythNet RPC host (optional, default is the PythNet RPC host)

If any of the following are not specified, the program will run in dry-run mode, outputting the metrics to stdout:

INFLUX_URL - InfluxDB URL
INFLUX_ORG - InfluxDB organization
INFLUX_USER - InfluxDB user (for token v1 authentication)
INFLUX_PASSWORD - InfluxDB password (for token v1 authentication)
INFLUX_BUCKET - Bucket to save the metrics to
`)
		os.Exit(0)
	}

	influxUrl := getEnv("INFLUX_URL", "", log)
	influxOrg := getEnv("INFLUX_ORG", "", log)
	influxUser := getEnv("INFLUX_USER", "", log)
	influxPass := getEnv("INFLUX_PASSWORD", "", log)
	influxBucket := getEnv("INFLUX_BUCKET", "", log)

	if influxUrl == "" || influxOrg == "" || influxUser == "" || influxPass == "" || influxBucket == "" {
		dryRun = true
	}

	if dryRun {
		log.Info("InfluxDB environment variables not set. Running in dry-run mode.")
	}

	rpcUrl := getEnv("RPC_URL", "https://api2.pythnet.pyth.network", log)

	pollStr := getEnv("POLL_SECONDS", "30", log)
	pollSecs, err := strconv.Atoi(pollStr)
	exitIfError(err, log)

	c := client.NewClient(rpcUrl)
	h := &http.Client{}

	// TODO We should persist the currentEpoch so that we continue correctly on restart instead of always writing one epoch
	currentEpoch := uint64(0)
	writeUrl := fmt.Sprintf("%s/write?org=%s&db=%s", influxUrl, influxOrg, influxBucket)

	ticker := time.NewTicker(time.Duration(pollSecs) * time.Second)

	for ; ; <-ticker.C {
		log.Debug("Starting loop")

		if currentEpoch == 0 {
			log.Debug("Initializing currentEpoch")
			epochInfo, err := c.GetEpochInfo(context.TODO())
			exitIfError(err, log)
			currentEpoch = epochInfo.Epoch
			log.Debug("currentEpoch initialized", "currentEpoch", currentEpoch)
		}

		blockProduction, err := getBlockProductionForEpoch(c, log, currentEpoch)
		if err != nil {
			log.Error("Error calling getBlockProduction", "error", err, "epoch", currentEpoch)
			continue
		}

		metrics := getMetrics(blockProduction, currentEpoch)

		log.Debug("Calling GetEpochInfo")
		epochInfo, err := c.GetEpochInfo(context.TODO())
		if err != nil {
			log.Error("Error calling GetEpochInfo", "error", err)
			continue
		}

		if epochInfo.Epoch != currentEpoch {
			log.Info("Switching epoch", "prev_epoch", currentEpoch, "new_epoch", epochInfo.Epoch)

			blockProduction, err := getBlockProductionForEpoch(c, log, currentEpoch)
			if err != nil {
				log.Error("Error calling getBlockProduction", "error", err, "epoch", currentEpoch)
				continue
			}

			metrics += getMetrics(blockProduction, currentEpoch)
		}

		if dryRun {
			fmt.Printf("Sending:\n%s\n\n", metrics)
			continue
		}

		rdr := strings.NewReader(metrics)
		req, err := http.NewRequest("POST", writeUrl, rdr)
		if err != nil {
			log.Error("Error creating InfluxDB POST request", "error", err, "writeUrl", writeUrl, "payload", metrics)
			continue
		}
		req.SetBasicAuth(influxUser, influxPass)

		log.Debug("POSTing to InfluxDB")
		resp, err := h.Do(req)
		if err != nil {
			log.Error("Error POSTing to InfluxDB", "error", err, "writeUrl", writeUrl, "user", influxUser, "payload", metrics)
			continue
		}

		// If there was an error when posting the data, do not move to the next epoch
		if resp.StatusCode >= 400 {
			body, err := io.ReadAll(resp.Body)
			exitIfError(err, log)

			log.Error("Error while talking to InfluxDB", "status", resp.StatusCode, "body", body)
			continue
		}

		currentEpoch = epochInfo.Epoch
		log.Debug("updating currentEpoch", "currentEpoch", currentEpoch)
	}
}
