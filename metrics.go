package main

import (
	"strconv"
	"strings"
)

// parseStatsLine parses a Psiphon conduit [STATS] log line into AppMetrics.
// Format: "[STATS] Connecting: 3 Connected: 12 Up: 1.50 GB Down: 3.20 GB Uptime: 2h 30m"
func parseStatsLine(line string) *AppMetrics {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil
	}

	metrics := &AppMetrics{}
	parsed := false

	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "Connecting:":
			if i+1 < len(fields) {
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					metrics.ConnectingClients = v
					parsed = true
					i++
				}
			}
		case "Connected:":
			if i+1 < len(fields) {
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					metrics.ConnectedClients = v
					parsed = true
					i++
				}
			}
		case "Up:":
			// Read value + unit tokens until next keyword or end
			val, unit, advance := readTrafficTokens(fields, i+1)
			if val != "" {
				metrics.BytesUploaded = parseTrafficValue(val, unit)
				parsed = true
			}
			i += advance
		case "Down:":
			val, unit, advance := readTrafficTokens(fields, i+1)
			if val != "" {
				metrics.BytesDownloaded = parseTrafficValue(val, unit)
				parsed = true
			}
			i += advance
		case "Uptime:":
			// Collect remaining tokens that look like duration parts
			var parts []string
			for j := i + 1; j < len(fields); j++ {
				if isStatsKeyword(fields[j]) {
					break
				}
				parts = append(parts, fields[j])
			}
			if len(parts) > 0 {
				metrics.UptimeSeconds = parseUptimeDuration(parts)
				parsed = true
			}
			i += len(parts)
		}
	}

	if !parsed {
		return nil
	}

	metrics.IsLive = true
	return metrics
}

// isStatsKeyword checks if a token is one of the [STATS] line keywords.
func isStatsKeyword(s string) bool {
	switch s {
	case "Connecting:", "Connected:", "Up:", "Down:", "Uptime:", "[STATS]":
		return true
	}
	return false
}

// readTrafficTokens reads a value and optional unit starting at fields[start].
// Returns (value, unit, tokensConsumed). Stops at next keyword or end.
func readTrafficTokens(fields []string, start int) (string, string, int) {
	if start >= len(fields) {
		return "", "", 0
	}

	val := fields[start]
	consumed := 1

	// Check if next token is a unit (not a keyword and not parseable as a number)
	if start+1 < len(fields) && !isStatsKeyword(fields[start+1]) {
		if _, err := strconv.ParseFloat(fields[start+1], 64); err != nil {
			return val, fields[start+1], 2
		}
	}

	return val, "B", consumed
}

// parseTrafficValue converts a value string and unit string to bytes.
func parseTrafficValue(valStr, unitStr string) float64 {
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0
	}

	unit := strings.ToUpper(strings.TrimSpace(unitStr))
	switch {
	case strings.HasPrefix(unit, "TB"):
		return val * 1099511627776
	case strings.HasPrefix(unit, "GB"):
		return val * 1073741824
	case strings.HasPrefix(unit, "MB"):
		return val * 1048576
	case strings.HasPrefix(unit, "KB"):
		return val * 1024
	default:
		return val
	}
}

// parseUptimeDuration converts duration parts like ["2h", "30m"] or ["1d", "5h", "30m"] to seconds.
func parseUptimeDuration(parts []string) float64 {
	var total float64
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if len(part) < 2 {
			continue
		}

		suffix := part[len(part)-1]
		numStr := part[:len(part)-1]
		val, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			continue
		}

		switch suffix {
		case 'd':
			total += val * 86400
		case 'h':
			total += val * 3600
		case 'm':
			total += val * 60
		case 's':
			total += val
		}
	}
	return total
}
