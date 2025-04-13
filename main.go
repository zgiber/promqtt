package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// --- Configuration ---

// Config holds all the configuration parameters for the service.
type Config struct {
	MQTTURL      string // Changed from MQTTBrokerURL
	MQTTTopic    string
	MQTTUser     string
	MQTTPass     string
	MQTTClientID string
	HTTPPort     int
}

// parseFlags parses command-line flags and populates the Config struct.
func parseFlags() (*Config, error) {
	// Changed: Single URL flag instead of schema/host/port
	mqttURL := flag.String("mqtt-url", "mqtt://localhost:1883", "MQTT broker URL (e.g., tcp://host:port, ws://host:port/mqtt)")
	// Changed: Default topic reflects 3 segments
	mqttTopic := flag.String("mqtt-topic", "tele/+/+", "MQTT topic to subscribe to (3 segments expected: topic/device/type)")
	mqttUser := flag.String("mqtt-user", "", "MQTT username (optional)")
	mqttPass := flag.String("mqtt-pass", "", "MQTT password (optional)")
	mqttClientID := flag.String("mqtt-client-id", "mqtt-prometheus-bridge-"+randomString(5), "MQTT Client ID (default includes random suffix)")
	httpPort := flag.Int("http-port", 9099, "Port for Prometheus metrics endpoint")

	flag.Parse()

	if *mqttURL == "" {
		return nil, errors.New("mqtt-url cannot be empty")
	}
	if *mqttTopic == "" {
		return nil, errors.New("mqtt-topic cannot be empty")
	}
	// Basic validation for topic structure could be added here if desired

	return &Config{
		MQTTURL:      *mqttURL, // Store the URL directly
		MQTTTopic:    *mqttTopic,
		MQTTUser:     *mqttUser,
		MQTTPass:     *mqttPass,
		MQTTClientID: *mqttClientID,
		HTTPPort:     *httpPort,
	}, nil
}

// --- Prometheus ---

// gaugeRegistry holds the dynamically created Prometheus GaugeVecs
// and the custom registry they are associated with.
type gaugeRegistry struct {
	vecs         map[string]*prometheus.GaugeVec
	lock         sync.RWMutex
	promRegistry prometheus.Registerer // Store the custom registry
	promFactory  promauto.Factory      // Store the promauto factory configured for the custom registry
}

// newGaugeRegistry creates a new registry for gauges associated with a specific prometheus registry.
func newGaugeRegistry(promReg prometheus.Registerer) *gaugeRegistry {
	return &gaugeRegistry{
		vecs:         make(map[string]*prometheus.GaugeVec),
		promRegistry: promReg,
		promFactory:  promauto.With(promReg),
	}
}

// metricLabels defines the standard labels applied to metrics.
// Changed: sensor label is now derived solely from JSON key, conceptually separated from topic parts
var metricLabels = []string{"topic", "device", "type", "sensor"}

// updateMetric finds or creates a GaugeVec (using the custom registry via the factory)
// and sets the value for the given labels.
func (r *gaugeRegistry) updateMetric(name string, help string, labels prometheus.Labels, value float64) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	vec, exists := r.vecs[name]
	if !exists {
		log.Printf("Creating new GaugeVec: %s with labels: %v (on custom registry)\n", name, metricLabels)
		vec = r.promFactory.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: name,
				Help: help,
			},
			metricLabels,
		)
		r.vecs[name] = vec
	}

	gauge, err := vec.GetMetricWith(labels)
	if err != nil {
		log.Printf("Error getting metric with labels for %s %v: %v\n", name, labels, err)
		return fmt.Errorf("failed to get metric '%s' with labels: %w", name, err)
	}

	gauge.Set(value)
	return nil
}

// startPrometheusServer starts the HTTP server for Prometheus metrics endpoint,
// using the provided custom registry.
func startPrometheusServer(port int, registry *prometheus.Registry) {
	listenAddr := fmt.Sprintf(":%d", port)
	log.Printf("Starting Prometheus metrics endpoint on %s/metrics (using custom registry)\n", listenAddr)

	mux := http.NewServeMux()
	promHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	mux.Handle("/metrics", promHandler)

	go func() {
		if err := http.ListenAndServe(listenAddr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start HTTP server: %v", err)
		}
	}()
}

// --- MQTT Message Processing ---

// topicInfo holds the parsed data from an MQTT topic.
type topicInfo struct {
	Labels prometheus.Labels
}

// parseTopic extracts labels from the topic string.
func parseTopic(topic string) (*topicInfo, error) {
	parts := strings.Split(topic, "/")
	// Changed: Check for exactly 3 parts
	if len(parts) != 3 {
		return nil, fmt.Errorf("topic '%s' needs exactly 3 segments (topic/device/type)", topic)
	}

	info := &topicInfo{
		Labels: prometheus.Labels{
			"topic":  parts[0],
			"device": parts[1],
			"type":   parts[2],
			// "sensor" label will be added later from JSON key
		},
	}

	// Validate that parts are not empty (optional but good practice)
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("topic segment %d is empty in topic '%s'", i+1, topic)
		}
	}

	return info, nil
}

// processJSONPayload parses the JSON and updates metrics via the registry.
func processJSONPayload(payload []byte, topicInfo *topicInfo, registry *gaugeRegistry) error {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("failed to unmarshal json payload: %w", err)
	}

	baseLabels := topicInfo.Labels // Base labels from topic (topic, device, type)

	for sensorKey, sensorData := range data {
		sensorMap, ok := sensorData.(map[string]any)
		if !ok {
			continue // Skip top-level fields that are not sensor objects
		}

		// Changed: sensorKey (e.g., "AM2301") IS the base name for the metric
		cleanedBaseName := cleanMetricName(sensorKey)
		if cleanedBaseName == "" {
			log.Printf("Warning: Empty metric base name derived from JSON key '%s'\n", sensorKey)
			continue
		}

		sensorLabelValue := cleanedBaseName // Use the cleaned base name also as sensor label

		// Create labels for this specific sensor block
		currentLabels := make(prometheus.Labels, len(baseLabels)+1)
		maps.Copy(currentLabels, baseLabels)
		currentLabels["sensor"] = sensorLabelValue // Add sensor label

		for metricSuffix, value := range sensorMap {
			floatValue, ok := convertToFloat64(value)
			if !ok {
				log.Printf("Skipping non-numeric value for sensor '%s', metric '%s': %v (type %T)\n", sensorKey, metricSuffix, value, value)
				continue
			}

			cleanedSuffix := cleanMetricName(metricSuffix)
			if cleanedSuffix == "" {
				log.Printf("Warning: Empty metric suffix derived from JSON key '%s' for sensor '%s'\n", metricSuffix, sensorKey)
				continue
			}

			// Changed: Construct full metric name from cleaned sensor key and cleaned suffix
			fullMetricName := fmt.Sprintf("%s_%s", cleanedBaseName, cleanedSuffix)

			// Changed: Updated help text to reflect new naming origin
			helpText := fmt.Sprintf("MQTT metric %s derived from JSON key %s", fullMetricName, sensorKey)

			// Update the metric in the registry
			err := registry.updateMetric(fullMetricName, helpText, currentLabels, floatValue)
			if err != nil {
				log.Printf("Error updating metric %s: %v", fullMetricName, err)
			}
		}
	}
	return nil
}

// onMessageReceived is the handler called by the MQTT client for incoming messages.
func onMessageReceived(registry *gaugeRegistry) mqtt.MessageHandler {
	return func(client mqtt.Client, msg mqtt.Message) {
		topic := msg.Topic()
		payload := msg.Payload()
		log.Printf("Received message on topic: %s (Payload size: %d bytes)\n", topic, len(payload))

		// 1. Parse topic (gets topic/device/type labels)
		topicInfo, err := parseTopic(topic)
		if err != nil {
			log.Printf("Error parsing topic '%s': %v\n", topic, err)
			return
		}

		// 2. Process payload (parses JSON, derives metric names & sensor label, updates registry)
		err = processJSONPayload(payload, topicInfo, registry)
		if err != nil {
			log.Printf("Error processing payload for topic '%s': %v\n", topic, err)
			return
		}
	}
}

// --- MQTT Client ---

// createMQTTClientOptions configures the MQTT client.
func createMQTTClientOptions(cfg *Config, registry *gaugeRegistry) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions()
	// Changed: Use the single URL from config
	opts.AddBroker(cfg.MQTTURL)
	opts.SetClientID(cfg.MQTTClientID)
	if cfg.MQTTUser != "" {
		opts.SetUsername(cfg.MQTTUser)
		opts.SetPassword(cfg.MQTTPass)
	}

	opts.SetDefaultPublishHandler(onMessageReceived(registry))
	opts.OnConnect = func(client mqtt.Client) {
		log.Println("MQTT Connected")
		subscribe(client, cfg.MQTTTopic)
	}
	opts.OnConnectionLost = func(client mqtt.Client, err error) {
		log.Printf("MQTT Connection Lost: %v. Attempting to reconnect...\n", err)
	}

	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetMaxReconnectInterval(1 * time.Minute)
	opts.SetCleanSession(true)

	return opts
}

// connectMQTT establishes the connection to the MQTT broker.
func connectMQTT(opts *mqtt.ClientOptions) (mqtt.Client, error) {
	client := mqtt.NewClient(opts)
	// Log the actual broker URL being used
	log.Printf("Attempting to connect to MQTT broker at %s\n", opts.Servers[0].String())
	if token := client.Connect(); token.WaitTimeout(10*time.Second) && token.Error() != nil {
		return nil, fmt.Errorf("failed to connect to MQTT broker: %w", token.Error())
	}
	if !client.IsConnected() {
		return nil, fmt.Errorf("failed to connect to MQTT broker (client reported not connected)")
	}
	return client, nil
}

// subscribe attempts to subscribe to the given topic.
func subscribe(client mqtt.Client, topic string) {
	qos := 1                                         // At least once delivery
	token := client.Subscribe(topic, byte(qos), nil) // Use default handler set in options

	go func() {
		if token.WaitTimeout(5*time.Second) && token.Error() != nil {
			log.Printf("Error subscribing to topic '%s': %v\n", topic, token.Error())
		} else if !token.WaitTimeout(1 * time.Second) {
			log.Printf("Subscription attempt to topic '%s' timed out\n", topic)
		} else {
			log.Printf("Successfully subscribed to topic: %s\n", topic)
		}
	}()
}

// --- Utilities ---

// Regex to clean metric names (replace invalid chars with underscores)
var metricNameRegex = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

// cleanMetricName converts a string to a Prometheus-compatible metric name.
func cleanMetricName(name string) string {
	name = strings.ToLower(name)
	name = metricNameRegex.ReplaceAllString(name, "_")
	// Remove leading/trailing underscores that might result from cleaning
	name = strings.Trim(name, "_")
	return name
}

// convertToFloat64 attempts to convert various numeric types to float64.
func convertToFloat64(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	case uint:
		return float64(val), true
	case uint32:
		return float64(val), true
	case uint64:
		return float64(val), true
	case json.Number:
		f, err := val.Float64()
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// randomString generates a simple random alphanumeric string.
func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	// Seeding with time is okay for non-crypto purposes like client IDs
	//nolint:gosec // G404: Use of weak random number generator (math/rand instead of crypto/rand) - acceptable for client ID
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}

// waitForShutdownSignal blocks until a SIGINT or SIGTERM is received.
func waitForShutdownSignal() {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	log.Printf("Service started successfully. Waiting for messages or shutdown signal (SIGINT/SIGTERM)...")
	s := <-quit
	log.Printf("Received signal: %s. Shutting down...", s)
}

// --- Main Orchestration ---

// Main function orchestrates the calls.
func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// 1. Configuration
	cfg, err := parseFlags()
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// 2. Setup Prometheus with a CUSTOM registry
	promRegistry := prometheus.NewRegistry()
	metricRegistry := newGaugeRegistry(promRegistry)
	startPrometheusServer(cfg.HTTPPort, promRegistry)

	// 3. Setup MQTT Client
	mqttOpts := createMQTTClientOptions(cfg, metricRegistry)
	client, err := connectMQTT(mqttOpts)
	if err != nil {
		log.Fatalf("Initial MQTT connection failed: %v", err)
	}

	// 4. Wait for shutdown signal
	waitForShutdownSignal()

	// 5. Graceful Shutdown
	log.Println("Disconnecting MQTT client...")
	client.Disconnect(2000)
	log.Println("MQTT client disconnected.")

	log.Println("Exiting.")
}
