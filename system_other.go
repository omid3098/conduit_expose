//go:build !linux

package main

func readDisk(rootPath string, m *SystemMetrics) {
	// Disk metrics only available on Linux via syscall.Statfs
}
