package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
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

// getContainerIP retrieves the first non-empty IP address from a container's networks.
func getContainerIP(ctx context.Context, cli *client.Client, inspect types.ContainerJSON) (string, error) {
	// If host network mode, the container shares the host's network stack
	if inspect.HostConfig != nil && inspect.HostConfig.NetworkMode == "host" {
		return "127.0.0.1", nil
	}

	if inspect.NetworkSettings != nil {
		for _, net := range inspect.NetworkSettings.Networks {
			if net.IPAddress != "" {
				return net.IPAddress, nil
			}
		}
	}

	return "", fmt.Errorf("no IP address found for container %s", inspect.ID[:12])
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

// fetchAppMetrics queries the Prometheus endpoint inside a container and parses the response.
func fetchAppMetrics(containerIP string, cfg *Config) (*AppMetrics, *ContainerSettings, error) {
	url := fmt.Sprintf("http://%s:%d%s", containerIP, cfg.MetricsPort, cfg.MetricsPath)

	httpClient := &http.Client{Timeout: cfg.MetricsTimeout}
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching metrics from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}

	appMetrics, settings := parsePrometheusMetrics(resp.Body)
	return appMetrics, settings, nil
}
