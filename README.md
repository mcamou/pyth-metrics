# pyth-metrics

Gathers PythNet metrics and pushes them to InfluxDB. At the moment it just gathers skipped slot metrics.

## Command-line flags

* `--dry-run=true` - Does not send the metrics to InfluxDB, just outputs them to stdout

## Environment variables

* `RPC_URL` - URL of the PythNet RPC host (optional, default is the PythNet RPC host)

The following are mandatory if `--dry-run=true` is NOT specified, otherwise they are unused

* `INFLUX_URL` - InfluxDB URL
* `INFLUX_ORG` - InfluxDB organization
* `INFLUX_USER` - InfluxDB user (for token v1 authentication)
* `INFLUX_PASSWORD` - InfluxDB password (for token v1 authentication)
* `INFLUX_BUCKET` - Bucket to save the metrics to

## Docker

The image can be found in [DockerHub](https://hub.docker.com/repository/docker/mcamou/pyth-metrics/general)
