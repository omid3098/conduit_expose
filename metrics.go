package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parsePrometheusMetrics reads Prometheus exposition format text and extracts
// conduit_* metrics and settings gauges.
func parsePrometheusMetrics(body io.Reader) (*AppMetrics, *ContainerSettings) {
	metrics := &AppMetrics{}
	settings := &ContainerSettings{}
	scanner := bufio.NewScanner(body)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, rest := splitMetricLine(line)

		switch name {
		case "conduit_connected_clients":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.ConnectedClients = int64(val)
			}
		case "conduit_connecting_clients":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.ConnectingClients = int64(val)
			}
		case "conduit_announcing":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.Announcing = int64(val)
			}
		case "conduit_is_live":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.IsLive = val >= 1
			}
		case "conduit_bytes_uploaded":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.BytesUploaded = val
			}
		case "conduit_bytes_downloaded":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.BytesDownloaded = val
			}
		case "conduit_uptime_seconds":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.UptimeSeconds = val
			}
		case "conduit_idle_seconds":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.IdleSeconds = val
			}
		case "conduit_max_clients":
			if val, err := parseMetricValue(rest); err == nil {
				settings.MaxClients = int(val)
			}
		case "conduit_bandwidth_limit_bytes_per_second":
			if val, err := parseMetricValue(rest); err == nil {
				// Convert bytes/sec to Mbps: val * 8 / 1_000_000
				settings.BandwidthLimitMbps = val * 8 / 1_000_000
			}
		}
	}

	return metrics, settings
}

// splitMetricLine splits "metric_name{labels} value" into name and the remainder.
func splitMetricLine(line string) (string, string) {
	if idx := strings.IndexByte(line, '{'); idx != -1 {
		return line[:idx], line[idx:]
	}
	if idx := strings.IndexByte(line, ' '); idx != -1 {
		return line[:idx], line[idx:]
	}
	return line, ""
}

// parseMetricValue extracts the float64 value from a string like " 42.5" or " 42.5 1625000000000".
func parseMetricValue(s string) (float64, error) {
	// Strip labels if present: "{...} value" → " value"
	if idx := strings.IndexByte(s, '}'); idx != -1 {
		s = s[idx+1:]
	}
	s = strings.TrimSpace(s)
	// Value may be followed by an optional timestamp; take just the first token
	if idx := strings.IndexByte(s, ' '); idx != -1 {
		s = s[:idx]
	}
	return strconv.ParseFloat(s, 64)
}
