package main

import (
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReadCMData reads all Conduit Manager data files and returns parsed results.
// If the CM directory doesn't exist, returns CMData{Available: false}.
// Individual file failures are logged but don't prevent other files from being read.
func ReadCMData(cfg *Config) *CMData {
	data := &CMData{}

	basePath := cfg.CMDataPath()
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return data
	}
	data.Available = true

	statsPath := cfg.CMTrafficStatsPath()

	// Active clients by country from tracker_snapshot (current 15s capture window).
	// NOT cumulative_ips which accumulates ALL IPs ever seen since tracker start.
	data.ClientsByCountry = readTrackerSnapshot(statsPath + "/tracker_snapshot")

	// Traffic by country from cumulative_data
	data.TrafficByCountry = readCumulativeData(statsPath + "/cumulative_data")

	// Peak connections
	data.TrackerStart, data.PeakConnections = readPeakConnections(statsPath + "/peak_connections")

	// Settings from settings.conf
	data.Settings = readCMSettings(basePath + "/settings.conf")

	return data
}

// safeReadLines reads a file and returns its lines.
// Discards the last line if it doesn't end with a newline (partial write protection).
// Returns nil on error (file missing, permission denied, etc.).
func safeReadLines(path string) []string {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	if len(content) == 0 {
		return nil
	}

	// If file doesn't end with newline, the last line may be a partial write
	endsWithNewline := content[len(content)-1] == '\n'

	lines := strings.Split(string(content), "\n")

	// Remove trailing empty string from Split
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	// Discard last line if file didn't end with newline (partial write)
	if !endsWithNewline && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}

	return lines
}

// readTrackerSnapshot parses traffic_stats/tracker_snapshot.
// Format: DIR|COUNTRY|BYTES|IP per line (e.g. "FROM|IR|102400|203.0.113.5").
// Counts unique IPs per country from the most recent capture window.
// This represents CURRENTLY active clients, matching what CM's TUI shows.
func readTrackerSnapshot(path string) []CountryStats {
	lines := safeReadLines(path)
	if len(lines) == 0 {
		return nil
	}

	// Count unique IPs per country
	countryIPs := make(map[string]map[string]struct{})
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		// DIR|COUNTRY|BYTES|IP
		country := strings.TrimSpace(parts[1])
		ip := strings.TrimSpace(parts[3])
		if country == "" || ip == "" {
			continue
		}
		if countryIPs[country] == nil {
			countryIPs[country] = make(map[string]struct{})
		}
		countryIPs[country][ip] = struct{}{}
	}

	// Convert to sorted slice
	result := make([]CountryStats, 0, len(countryIPs))
	for country, ips := range countryIPs {
		result = append(result, CountryStats{
			Country:     country,
			Connections: len(ips),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Connections > result[j].Connections
	})

	return result
}

// readCumulativeData parses traffic_stats/cumulative_data.
// Format: COUNTRY|FROM_BYTES|TO_BYTES per line.
func readCumulativeData(path string) []CountryTrafficStats {
	lines := safeReadLines(path)
	if len(lines) == 0 {
		return nil
	}

	// Aggregate by country (in case of duplicate entries)
	countryTraffic := make(map[string]*CountryTrafficStats)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		country := strings.TrimSpace(parts[0])
		if country == "" {
			continue
		}
		fromBytes, err1 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		toBytes, err2 := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		if err1 != nil || err2 != nil {
			continue
		}

		if ct, ok := countryTraffic[country]; ok {
			ct.FromBytes += fromBytes
			ct.ToBytes += toBytes
		} else {
			countryTraffic[country] = &CountryTrafficStats{
				Country:   country,
				FromBytes: fromBytes,
				ToBytes:   toBytes,
			}
		}
	}

	result := make([]CountryTrafficStats, 0, len(countryTraffic))
	for _, ct := range countryTraffic {
		result = append(result, *ct)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].FromBytes+result[i].ToBytes > result[j].FromBytes+result[j].ToBytes
	})

	return result
}

// readPeakConnections parses traffic_stats/peak_connections.
// Format: line 1 = container start timestamp, line 2 = peak value.
func readPeakConnections(path string) (time.Time, int64) {
	lines := safeReadLines(path)
	if len(lines) < 2 {
		return time.Time{}, 0
	}

	startTime, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[0]))
	if err != nil {
		// Try Unix timestamp as fallback
		if ts, tsErr := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64); tsErr == nil {
			startTime = time.Unix(ts, 0)
		}
	}

	peak, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err != nil {
		return startTime, 0
	}

	return startTime, peak
}

// readCMSettings parses settings.conf (bash KEY=VALUE format).
func readCMSettings(path string) *CMSettings {
	lines := safeReadLines(path)
	if len(lines) == 0 {
		return nil
	}

	settings := &CMSettings{}
	found := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "MAX_CLIENTS":
			if v, err := strconv.Atoi(val); err == nil {
				settings.MaxClients = v
				found = true
			}
		case "BANDWIDTH":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				settings.Bandwidth = v
				found = true
			}
		case "CONTAINER_COUNT":
			if v, err := strconv.Atoi(val); err == nil {
				settings.ContainerCount = v
				found = true
			}
		case "SNOWFLAKE_ENABLED":
			settings.SnowflakeEnabled = strings.EqualFold(val, "true")
			found = true
		case "SNOWFLAKE_COUNT":
			if v, err := strconv.Atoi(val); err == nil {
				settings.SnowflakeCount = v
				found = true
			}
		}
	}

	if !found {
		log.Printf("WARN: settings.conf at %s has no recognized settings", path)
		return nil
	}

	return settings
}
