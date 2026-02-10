package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// discoverContainers finds all containers matching the conduit image or name prefix,
// excluding the conduit-expose container itself.
func discoverContainers(ctx context.Context, cli *client.Client) ([]types.Container, error) {
	seen := make(map[string]types.Container)

	// Pass 1: filter by image (ancestor)
	imageFilter := filters.NewArgs(filters.Arg("ancestor", conduitImage))
	imageContainers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: imageFilter,
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers by image: %w", err)
	}
	for _, c := range imageContainers {
		seen[c.ID] = c
	}

	// Pass 2: filter by name prefix
	nameFilter := filters.NewArgs(filters.Arg("name", conduitName))
	nameContainers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: nameFilter,
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers by name: %w", err)
	}
	for _, c := range nameContainers {
		seen[c.ID] = c
	}

	// Self-filtering: exclude our own container
	hostname, _ := os.Hostname()

	result := make([]types.Container, 0, len(seen))
	for _, c := range seen {
		// Docker sets hostname to the container ID (first 12 chars)
		if hostname != "" && strings.HasPrefix(c.ID, hostname) {
			continue
		}
		// Also skip by name as fallback
		name := containerName(c)
		if name == "conduit-expose" {
			continue
		}
		result = append(result, c)
	}
	return result, nil
}

// containerName returns the cleaned name of a container.
func containerName(c types.Container) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return ""
}

// fetchAppMetricsFromLogs reads a container's recent logs via the Docker API,
// finds the last [STATS] line, and parses it for app-level metrics.
func fetchAppMetricsFromLogs(ctx context.Context, cli *client.Client, containerID string, cfg *Config) (*AppMetrics, error) {
	logsCtx, cancel := context.WithTimeout(ctx, cfg.DockerTimeout)
	defer cancel()

	reader, err := cli.ContainerLogs(logsCtx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "200",
	})
	if err != nil {
		return nil, fmt.Errorf("reading container logs: %w", err)
	}
	defer reader.Close()

	// Docker multiplexed stream has 8-byte header per frame.
	// Read all content, stripping headers.
	var lastStatsLine string
	br := bufio.NewReader(reader)
	for {
		// Read 8-byte header: [stream_type(1), 0, 0, 0, size(4)]
		header := make([]byte, 8)
		_, err := io.ReadFull(br, header)
		if err != nil {
			break
		}
		frameSize := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if frameSize <= 0 {
			continue
		}
		frame := make([]byte, frameSize)
		_, err = io.ReadFull(br, frame)
		if err != nil {
			break
		}

		// Frame may contain multiple lines
		for _, line := range strings.Split(string(frame), "\n") {
			if strings.Contains(line, "[STATS]") {
				lastStatsLine = line
			}
		}
	}

	if lastStatsLine == "" {
		return nil, nil
	}

	metrics := parseStatsLine(lastStatsLine)
	return metrics, nil
}

// containerUptimeSeconds computes seconds since container started from inspect data.
func containerUptimeSeconds(inspect types.ContainerJSON) float64 {
	if inspect.State == nil || inspect.State.StartedAt == "" {
		return 0
	}
	started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
	if err != nil {
		return 0
	}
	return time.Since(started).Seconds()
}

// collectContainerStats gathers Docker stats for a single container.
func collectContainerStats(ctx context.Context, cli *client.Client, ctr types.Container, cfg *Config) ContainerInfo {
	name := containerName(ctr)

	info := ContainerInfo{
		ID:     ctr.ID[:12],
		Name:   name,
		Status: ctr.State,
		Uptime: "0s",
	}

	if ctr.State != "running" {
		info.Status = "down"
		return info
	}

	info.Uptime = time.Since(time.Unix(ctr.Created, 0)).Truncate(time.Second).String()

	statsCtx, cancel := context.WithTimeout(ctx, cfg.DockerTimeout)
	defer cancel()

	statsResp, err := cli.ContainerStats(statsCtx, ctr.ID, false)
	if err != nil {
		log.Printf("WARN: failed to get stats for %s: %v", name, err)
		info.Status = "unhealthy"
		return info
	}
	defer statsResp.Body.Close()

	var stats container.StatsResponse
	if err := json.NewDecoder(statsResp.Body).Decode(&stats); err != nil {
		log.Printf("WARN: failed to decode stats for %s: %v", name, err)
		info.Status = "unhealthy"
		return info
	}

	// CPU percentage calculation
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
	numCPU := float64(stats.CPUStats.OnlineCPUs)
	if numCPU == 0 {
		numCPU = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if numCPU == 0 {
		numCPU = 1
	}
	if systemDelta > 0 && cpuDelta >= 0 {
		info.CPUPercent = math.Round((cpuDelta/systemDelta)*numCPU*100.0*100) / 100
	}

	// Memory in MB
	info.MemoryMB = math.Round(float64(stats.MemoryStats.Usage)/1024/1024*100) / 100

	return info
}

// collectContainerHealth extracts health indicators from a Docker inspect result
// and process info from /proc.
func collectContainerHealth(inspect types.ContainerJSON, hostProcPath string) *ContainerHealth {
	health := &ContainerHealth{}

	health.RestartCount = inspect.RestartCount
	if inspect.State != nil {
		health.OOMKilled = inspect.State.OOMKilled
	}

	pid := inspect.State.Pid
	if pid <= 0 {
		return health
	}

	// Count open file descriptors
	fdPath := fmt.Sprintf("%s/%d/fd", hostProcPath, pid)
	if entries, err := os.ReadDir(fdPath); err == nil {
		health.FDCount = len(entries)
	}

	// Read thread count from /proc/<pid>/status
	statusPath := fmt.Sprintf("%s/%d/status", hostProcPath, pid)
	if f, err := os.Open(statusPath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Threads:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					fmt.Sscanf(fields[1], "%d", &health.ThreadCount)
				}
				break
			}
		}
	}

	return health
}

// extractContainerSettings reads the restart policy from Docker inspect
// and returns the AutoStart field.
func extractAutoStart(inspect types.ContainerJSON) bool {
	if inspect.HostConfig == nil {
		return false
	}
	policy := inspect.HostConfig.RestartPolicy.Name
	return policy == "always" || policy == "unless-stopped"
}

