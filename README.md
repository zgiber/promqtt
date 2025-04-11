# promqtt - MQTT to Prometheus Bridge

A simple Go service that connects to an MQTT broker, subscribes
to a specified topic structure, parses JSON payloads, and exposes the data as
Prometheus metrics via an HTTP endpoint.

## Overview

This tool acts as a bridge between MQTT messages (typically from IoT devices
like those running Tasmota, ESPHome, etc.) and a Prometheus monitoring system.
It dynamically creates Prometheus gauges based on the MQTT topic structure and
the fields within the JSON payload of the messages. 

## Limitations

Only tested with Tasmota. Only extracts gauge values from numeric measurements.

## Features

* Connects to an MQTT broker using various schemas (mqtt, tcp, ws, wss, ssl).
* Subscribes to MQTT topics with wildcard support (`+`, `#`).
* Expects a specific 3-segment topic structure (`<topic_label>/<device_label>/<type_label>`).
* Parses JSON payloads from MQTT messages.
* Dynamically creates Prometheus Gauge metrics.
  * Metric Name: Derived from JSON keys (`<sensor_key>_<metric_key>`).
  * Labels: Includes `topic`, `device`, `type` (from MQTT topic segments) and
  `sensor` (from JSON sensor key).
* Exposes metrics on a configurable HTTP port (`/metrics`) for Prometheus scraping.
* Can be run directly or as a systemd background service.
* Exposes only custom metrics (no default Go/process metrics).

An example collected metric:

|Element|Value|
|-------|-----|
|scd40_carbondioxide{device="tasmota_5E2D5C",instance="localhost:9099",job="tasmota_mqtt",sensor="scd40",topic="tele",type="SENSOR"}|1326|

## Requirements

* **Go Compiler:** Required to build the binary from source (Go 1.18+ recommended).
* **MQTT Broker:** An accessible MQTT broker where your devices publish data.
* **Prometheus:** A running Prometheus instance to scrape the metrics exposed
by this tool.

## Installation (from Source)

1. **Clone/Download:** Get the source code (assuming you have the `.go` file).
2. **Build:** Open your terminal in the directory containing the source code
(`main.go`) and run:

    ```bash
    go build -o promqtt .
    ```

    This will create an executable file named `promqtt` in the current directory.
3. **Place Binary (Optional but recommended for service):** Move the compiled
binary to a standard location:

    ```bash
    sudo mv promqtt /usr/local/bin/promqtt
    sudo chown root:root /usr/local/bin/promqtt
    sudo chmod 755 /usr/local/bin/promqtt
    ```

## Configuration

`promqtt` is configured via command-line flags:

* `-mqtt-url`: (Required) The full URL of your MQTT broker.
  * Examples: `mqtt://localhost:1883`, `ssl://mqtt.example.com:8883`, `ws://broker.local:9001/mqtt`
* `-mqtt-topic`: (Required) The MQTT topic pattern to subscribe to. Wildcards
(`+`, `#`) are allowed. **Must resolve to 3 segments** for label mapping (`topic/device/type`).
  * Example: `tele/+/+`, `sensors/+/status`
* `-http-port`: (Optional) The port number for the Prometheus metrics HTTP endpoint.
  * Default: `9099`
* `-mqtt-user`: (Optional) Username for MQTT broker authentication.
* `-mqtt-pass`: (Optional) Password for MQTT broker authentication.
* `-mqtt-client-id`: (Optional) Custom MQTT client ID.
  * Default: `mqtt-prometheus-bridge-` followed by 5 random characters.

## Usage

### 1. Running Directly

You can run the binary directly from your terminal, providing the necessary flags:

```bash
/usr/local/bin/promqtt \
    -mqtt-url "mqtt://your_broker_host:1883" \
    -mqtt-topic "tele/+/+" \
    -http-port 9099 \
    -mqtt-user "your_user" \
    -mqtt-pass "your_password"
