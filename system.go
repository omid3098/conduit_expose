package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Package-level state for delta-based metrics
// ============================================================

var (
	sysMu       sync.Mutex
	prevCPU     *cpuSample
	prevNet     *netSample
	prevNetTime time.Time
)

type cpuSample struct {
	idle  uint64
	total uint64
}

type netSample struct {
	rxBytes   uint64
	txBytes   uint64
	rxErrors  uint64
	txErrors  uint64
	rxDropped uint64
	txDropped uint64
}

// collectSystemMetrics reads host-level metrics from /proc and the root filesystem.
// Returns nil if the host proc path doesn't exist (graceful degradation).
func collectSystemMetrics(cfg *Config) *SystemMetrics {
	if _, err := os.Stat(cfg.HostProcPath); os.IsNotExist(err) {
		return nil
	}

	m := &SystemMetrics{}

	sysMu.Lock()
	defer sysMu.Unlock()

	readCPU(cfg.HostProcPath, m)
	readMemory(cfg.HostProcPath, m)
	readLoadAvg(cfg.HostProcPath, m)
	readDisk(cfg.HostRootPath, m)
	readNetwork(cfg.HostProcPath, m)

	return m
}

// readCPU parses /proc/stat for host CPU usage.
func readCPU(procPath string, m *SystemMetrics) {
	f, err := os.Open(fmt.Sprintf("%s/stat", procPath))
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return
	}

	line := scanner.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return
	}

	fields := strings.Fields(line)
	if len(fields) < 5 {
		return
	}

	// Fields: cpu user nice system idle iowait irq softirq steal guest guest_nice
	var values [10]uint64
	for i := 1; i < len(fields) && i <= 10; i++ {
		values[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
	}

	var total uint64
	for _, v := range values {
		total += v
	}
	idle := values[3] // idle is the 4th field (index 3)

	current := &cpuSample{idle: idle, total: total}

	if prevCPU != nil {
		totalDelta := float64(current.total - prevCPU.total)
		idleDelta := float64(current.idle - prevCPU.idle)
		if totalDelta > 0 {
			m.CPUPercent = math.Round((1-idleDelta/totalDelta)*100*100) / 100
		}
	}

	prevCPU = current
}

// readMemory parses /proc/meminfo for host memory usage.
func readMemory(procPath string, m *SystemMetrics) {
	f, err := os.Open(fmt.Sprintf("%s/meminfo", procPath))
	if err != nil {
		return
	}
	defer f.Close()

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			memTotal = parseMemInfoValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			memAvailable = parseMemInfoValue(line)
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}

	if memTotal > 0 {
		m.MemoryTotalMB = math.Round(float64(memTotal)/1024*100) / 100
		m.MemoryUsedMB = math.Round(float64(memTotal-memAvailable)/1024*100) / 100
	}
}

// parseMemInfoValue extracts the numeric value in kB from a /proc/meminfo line.
func parseMemInfoValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, _ := strconv.ParseUint(fields[1], 10, 64)
	return val
}

// readLoadAvg parses /proc/loadavg for system load averages.
func readLoadAvg(procPath string, m *SystemMetrics) {
	data, err := os.ReadFile(fmt.Sprintf("%s/loadavg", procPath))
	if err != nil {
		return
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return
	}

	m.LoadAvg1m, _ = strconv.ParseFloat(fields[0], 64)
	m.LoadAvg5m, _ = strconv.ParseFloat(fields[1], 64)
	m.LoadAvg15m, _ = strconv.ParseFloat(fields[2], 64)
}

// readDisk is implemented in system_linux.go (uses syscall.Statfs).

// readNetwork parses /proc/net/dev for host network throughput and errors.
// Uses PID 1's network namespace to get host-level stats even when running
// inside a container (/proc/net is per-namespace via /proc/self/net symlink).
func readNetwork(procPath string, m *SystemMetrics) {
	f, err := os.Open(fmt.Sprintf("%s/1/net/dev", procPath))
	if err != nil {
		return
	}
	defer f.Close()

	current := &netSample{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		// Lines look like: "  eth0: 12345 ... "
		colonIdx := strings.IndexByte(line, ':')
		if colonIdx < 0 {
			continue
		}

		iface := strings.TrimSpace(line[:colonIdx])
		if iface == "lo" {
			continue
		}

		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 10 {
			continue
		}

		// Fields: rx_bytes rx_packets rx_errs rx_drop ... tx_bytes tx_packets tx_errs tx_drop ...
		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		rxErrors, _ := strconv.ParseUint(fields[2], 10, 64)
		rxDropped, _ := strconv.ParseUint(fields[3], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		txErrors, _ := strconv.ParseUint(fields[10], 10, 64)
		txDropped, _ := strconv.ParseUint(fields[11], 10, 64)

		current.rxBytes += rxBytes
		current.txBytes += txBytes
		current.rxErrors += rxErrors
		current.txErrors += txErrors
		current.rxDropped += rxDropped
		current.txDropped += txDropped
	}

	now := time.Now()
	if prevNet != nil && !prevNetTime.IsZero() {
		elapsed := now.Sub(prevNetTime).Seconds()
		if elapsed > 0 {
			rxDelta := float64(current.rxBytes - prevNet.rxBytes)
			txDelta := float64(current.txBytes - prevNet.txBytes)
			m.NetInMbps = math.Round(rxDelta*8/(elapsed*1e6)*100) / 100
			m.NetOutMbps = math.Round(txDelta*8/(elapsed*1e6)*100) / 100

			m.NetErrors = int64(current.rxErrors - prevNet.rxErrors + current.txErrors - prevNet.txErrors)
			m.NetDrops = int64(current.rxDropped - prevNet.rxDropped + current.txDropped - prevNet.txDropped)
		}
	}

	prevNet = current
	prevNetTime = now
}
