//go:build linux

package main

import (
	"log"
	"math"
	"os"
	"syscall"
)

func readDisk(rootPath string, m *SystemMetrics) {
	if _, err := os.Stat(rootPath); os.IsNotExist(err) {
		return
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(rootPath, &stat); err != nil {
		log.Printf("WARN: failed to statfs %s: %v", rootPath, err)
		return
	}

	bsize := uint64(stat.Bsize)
	m.DiskTotalGB = math.Round(float64(stat.Blocks*bsize)/1e9*100) / 100
	m.DiskUsedGB = math.Round(float64((stat.Blocks-stat.Bfree)*bsize)/1e9*100) / 100
}
