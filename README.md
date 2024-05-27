# pyth-metrics

Gathers PythNet metrics and pushes them to InfluxDB. At the moment it just gathers skipped slot metrics.

## Command-line flags

* `--help` - Show the help text

## Environment variables

* `RPC_URL` - URL of the PythNet RPC host (optional, default is the PythNet RPC host)

If any of the following are not specified, the program will run in dry-run mode:

* `INFLUX_URL` - InfluxDB URL
* `INFLUX_ORG` - InfluxDB organization
* `INFLUX_USER` - InfluxDB user (for token v1 authentication)
* `INFLUX_PASSWORD` - InfluxDB password (for token v1 authentication)
* `INFLUX_BUCKET` - Bucket to save the metrics to

This means that:

* Nothing will be sent to InfluxDB
* The metrics will be output to stdout

## Docker

The image can be found in [DockerHub](https://hub.docker.com/repository/docker/mcamou/pyth-metrics/general)
