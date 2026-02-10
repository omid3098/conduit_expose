package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// TCP states from /proc/net/tcp (hex state code → name).
var tcpStates = map[string]string{
	"01": "established",
	"02": "syn_sent",
	"03": "syn_recv",
	"04": "fin_wait1",
	"05": "fin_wait2",
	"06": "time_wait",
	"07": "close",
	"08": "close_wait",
	"09": "last_ack",
	"0A": "listen",
	"0B": "closing",
}

// collectContainerConnections reads TCP connections from a container's network namespace
// via /proc/<pid>/net/tcp and /proc/<pid>/net/tcp6, then aggregates connection states
// and resolves remote IPs to countries.
func collectContainerConnections(hostProcPath string, pid int, geo *GeoIPResolver) (*ConnectionStats, []CountryStats) {
	stats := &ConnectionStats{
		States: make(map[string]int),
	}
	uniqueIPs := make(map[string]struct{})
	countryCounts := make(map[string]int)

	// Parse both IPv4 and IPv6 TCP connection tables
	for _, proto := range []string{"tcp", "tcp6"} {
		path := fmt.Sprintf("%s/%d/net/%s", hostProcPath, pid, proto)
		entries, err := parseProcNetTCP(path, proto == "tcp6")
		if err != nil {
			continue
		}

		for _, e := range entries {
			// Skip listening sockets and loopback
			if e.remotePort == 0 {
				continue
			}
			if e.remoteIP.IsLoopback() {
				continue
			}

			stats.Total++

			if stateName, ok := tcpStates[e.state]; ok {
				stats.States[stateName]++
			}

			ipStr := e.remoteIP.String()
			uniqueIPs[ipStr] = struct{}{}

			// Only count ESTABLISHED connections for country stats
			// to match what Conduit Manager shows as "Active Clients"
			if e.state == "01" && geo != nil {
				country := geo.Lookup(e.remoteIP)
				if country != "" {
					countryCounts[country]++
				}
			}
		}
	}

	stats.UniqueIPs = len(uniqueIPs)

	// Convert country map to sorted slice
	var countries []CountryStats
	for code, count := range countryCounts {
		countries = append(countries, CountryStats{Country: code, Connections: count})
	}
	sort.Slice(countries, func(i, j int) bool {
		return countries[i].Connections > countries[j].Connections
	})

	return stats, countries
}

// tcpEntry represents a single parsed line from /proc/net/tcp{,6}.
type tcpEntry struct {
	remoteIP   net.IP
	remotePort uint16
	state      string // hex state code, e.g. "01"
}

// parseProcNetTCP parses /proc/<pid>/net/tcp or tcp6 and returns connection entries.
func parseProcNetTCP(path string, isIPv6 bool) ([]tcpEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []tcpEntry
	scanner := bufio.NewScanner(f)

	// Skip header line
	if !scanner.Scan() {
		return nil, nil
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Format: sl local_address rem_address st tx_queue:rx_queue ...
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		remAddr := fields[2]  // e.g. "0100007F:0050" or hex IPv6
		state := fields[3]    // e.g. "01"

		ip, port, err := parseHexAddr(remAddr, isIPv6)
		if err != nil {
			continue
		}

		entries = append(entries, tcpEntry{
			remoteIP:   ip,
			remotePort: port,
			state:      state,
		})
	}

	return entries, nil
}

// parseHexAddr parses a hex-encoded address like "0100007F:0050" into IP and port.
// IPv4 addresses are little-endian; IPv6 addresses are stored as 4 groups of 4 bytes, each little-endian.
func parseHexAddr(s string, isIPv6 bool) (net.IP, uint16, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return nil, 0, fmt.Errorf("invalid address format: %s", s)
	}

	hexIP := parts[0]
	hexPort := parts[1]

	// Parse port (big-endian)
	portBytes, err := hex.DecodeString(hexPort)
	if err != nil || len(portBytes) != 2 {
		return nil, 0, fmt.Errorf("invalid port: %s", hexPort)
	}
	port := uint16(portBytes[0])<<8 | uint16(portBytes[1])

	// Parse IP
	ipBytes, err := hex.DecodeString(hexIP)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid IP hex: %s", hexIP)
	}

	var ip net.IP
	if isIPv6 {
		if len(ipBytes) != 16 {
			return nil, 0, fmt.Errorf("invalid IPv6 length: %d", len(ipBytes))
		}
		// IPv6 in /proc is stored as 4 groups of 4 bytes, each group little-endian
		ip = make(net.IP, 16)
		for g := 0; g < 4; g++ {
			off := g * 4
			ip[off+0] = ipBytes[off+3]
			ip[off+1] = ipBytes[off+2]
			ip[off+2] = ipBytes[off+1]
			ip[off+3] = ipBytes[off+0]
		}
	} else {
		if len(ipBytes) != 4 {
			return nil, 0, fmt.Errorf("invalid IPv4 length: %d", len(ipBytes))
		}
		// IPv4 in /proc is little-endian
		ip = net.IPv4(ipBytes[3], ipBytes[2], ipBytes[1], ipBytes[0])
	}

	return ip, port, nil
}

// mergeConnectionStats merges connection stats from multiple containers.
func mergeConnectionStats(all []*ConnectionStats) *ConnectionStats {
	merged := &ConnectionStats{
		States: make(map[string]int),
	}
	seenIPs := make(map[string]struct{})

	for _, s := range all {
		if s == nil {
			continue
		}
		merged.Total += s.Total
		for state, count := range s.States {
			merged.States[state] += count
		}
		// UniqueIPs can't be simply summed (overlap), but we approximate as the sum
		// since containers have separate network namespaces
		merged.UniqueIPs += s.UniqueIPs
	}
	_ = seenIPs

	return merged
}

// mergeCountryStats merges country stats from multiple containers.
func mergeCountryStats(all [][]CountryStats) []CountryStats {
	counts := make(map[string]int)
	for _, list := range all {
		for _, cs := range list {
			counts[cs.Country] += cs.Connections
		}
	}

	var result []CountryStats
	for code, count := range counts {
		result = append(result, CountryStats{Country: code, Connections: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Connections > result[j].Connections
	})

	return result
}
