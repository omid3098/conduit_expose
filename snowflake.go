package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// collectSnowflakeMetrics scrapes Prometheus /internal/metrics from snowflake
// proxy containers running on the host. Snowflake containers expose metrics
// on host ports starting at 10000 (snowflake-1 → 10000, snowflake-2 → 9999).
// Since conduit-expose runs with --network=host, it can access these directly.
func collectSnowflakeMetrics(ctx context.Context, cfg *Config) *SnowflakeMetrics {
	snowflakeCount := 1
	if cfg != nil {
		// The caller already checked settings; default to 1 container
	}

	// Read settings to determine snowflake count
	cmSettings := readCMSettings(cfg.CMDataPath() + "/settings.conf")
	if cmSettings != nil && cmSettings.SnowflakeCount > 0 {
		snowflakeCount = cmSettings.SnowflakeCount
	}

	aggregated := &SnowflakeMetrics{}
	found := false

	for i := 1; i <= snowflakeCount; i++ {
		port := 10001 - i // snowflake-1 → 10000, snowflake-2 → 9999
		addr := fmt.Sprintf("http://127.0.0.1:%d/internal/metrics", port)

		metrics, err := scrapeSnowflakePrometheus(ctx, addr)
		if err != nil {
			log.Printf("WARN: snowflake-%d metrics unavailable at %s: %v", i, addr, err)
			continue
		}

		aggregated.TotalConnections += metrics.TotalConnections
		aggregated.TimeoutsTotal += metrics.TimeoutsTotal
		aggregated.InboundBytes += metrics.InboundBytes
		aggregated.OutboundBytes += metrics.OutboundBytes
		found = true
	}

	if !found {
		return nil
	}

	return aggregated
}

// scrapeSnowflakePrometheus fetches and parses Prometheus text format from
// a single snowflake container.
func scrapeSnowflakePrometheus(ctx context.Context, addr string) (*SnowflakeMetrics, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", addr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return parsePrometheusText(resp.Body)
}

// parsePrometheusText extracts snowflake-specific metrics from Prometheus
// text exposition format. This is a minimal parser for the specific metrics
// we need, not a full Prometheus client.
func parsePrometheusText(reader io.Reader) (*SnowflakeMetrics, error) {
	metrics := &SnowflakeMetrics{}
	scanner := bufio.NewScanner(reader)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Parse metric lines: metric_name{labels} value
		// or: metric_name value
		switch {
		case strings.HasPrefix(line, "tor_snowflake_proxy_connections_total"):
			// May have labels like {country="US"} — sum all
			if val := extractPrometheusValue(line); val > 0 {
				metrics.TotalConnections += int64(val)
			}

		case strings.HasPrefix(line, "tor_snowflake_proxy_connection_timeouts_total"):
			if val := extractPrometheusValue(line); val > 0 {
				metrics.TimeoutsTotal += int64(val)
			}

		case strings.HasPrefix(line, "tor_snowflake_proxy_traffic_inbound_bytes_total"):
			if val := extractPrometheusValue(line); val > 0 {
				metrics.InboundBytes += val
			}

		case strings.HasPrefix(line, "tor_snowflake_proxy_traffic_outbound_bytes_total"):
			if val := extractPrometheusValue(line); val > 0 {
				metrics.OutboundBytes += val
			}
		}
	}

	return metrics, scanner.Err()
}

// extractPrometheusValue extracts the numeric value from a Prometheus metric line.
// Handles both "metric_name value" and "metric_name{labels} value" formats.
func extractPrometheusValue(line string) float64 {
	// The value is always the last space-separated token
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}

	val, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		return 0
	}

	return val
}
