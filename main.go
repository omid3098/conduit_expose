package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ============================================================
// Constants
// ============================================================

const (
	defaultListenAddr     = ":8081"
	defaultMetricsPort    = 9090
	defaultMetricsPath    = "/metrics"
	defaultPollInterval   = 15 * time.Second
	defaultDockerTimeout  = 5 * time.Second
	defaultMetricsTimeout = 3 * time.Second
	defaultMaxWorkers     = 10

	conduitImage = "ghcr.io/psiphon-inc/conduit/cli"
	conduitName  = "conduit"
)

// ============================================================
// Type Definitions
// ============================================================

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	ListenAddr     string
	AuthSecret     string
	MetricsPort    int
	MetricsPath    string
	PollInterval   time.Duration
	DockerTimeout  time.Duration
	MetricsTimeout time.Duration
	MaxWorkers     int
}

// AppMetrics holds parsed Prometheus metrics from a single conduit container.
type AppMetrics struct {
	Connections int64   `json:"connections"`
	TrafficIn   float64 `json:"traffic_in"`
	TrafficOut  float64 `json:"traffic_out"`
}

// ContainerInfo represents a single container's collected data.
type ContainerInfo struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	CPUPercent float64     `json:"cpu_percent"`
	MemoryMB   float64     `json:"memory_mb"`
	Uptime     string      `json:"uptime"`
	AppMetrics *AppMetrics `json:"app_metrics"`
}

// StatusResponse is the top-level JSON response for GET /status.
type StatusResponse struct {
	ServerID        string          `json:"server_id"`
	Timestamp       int64           `json:"timestamp"`
	TotalContainers int             `json:"total_containers"`
	Containers      []ContainerInfo `json:"containers"`
}

// StatusCache provides thread-safe access to the latest StatusResponse.
type StatusCache struct {
	mu       sync.RWMutex
	response *StatusResponse
}

func (c *StatusCache) Get() *StatusResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.response
}

func (c *StatusCache) Set(r *StatusResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.response = r
}

// ============================================================
// Configuration Loader
// ============================================================

func loadConfig() *Config {
	cfg := &Config{
		ListenAddr:     envOrDefault("CONDUIT_LISTEN_ADDR", defaultListenAddr),
		AuthSecret:     os.Getenv("CONDUIT_AUTH_SECRET"),
		MetricsPort:    envIntOrDefault("CONDUIT_METRICS_PORT", defaultMetricsPort),
		MetricsPath:    envOrDefault("CONDUIT_METRICS_PATH", defaultMetricsPath),
		PollInterval:   envDurationOrDefault("CONDUIT_POLL_INTERVAL", defaultPollInterval),
		DockerTimeout:  defaultDockerTimeout,
		MetricsTimeout: defaultMetricsTimeout,
		MaxWorkers:     defaultMaxWorkers,
	}
	return cfg
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("WARN: invalid integer for %s=%q, using default %d", key, v, fallback)
		return fallback
	}
	return n
}

func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("WARN: invalid duration for %s=%q, using default %s", key, v, fallback)
		return fallback
	}
	return d
}

// ============================================================
// Prometheus Text Parser
// ============================================================

// parsePrometheusMetrics reads Prometheus exposition format text and extracts
// only the metrics we care about: active_connections and bytes_transferred_total.
func parsePrometheusMetrics(body io.Reader) *AppMetrics {
	metrics := &AppMetrics{}
	scanner := bufio.NewScanner(body)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments/TYPE/HELP
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Extract metric name (everything before '{' or first space)
		name, rest := splitMetricLine(line)

		switch {
		case name == "active_connections":
			if val, err := parseMetricValue(rest); err == nil {
				metrics.Connections = int64(val)
			}
		case name == "bytes_transferred_total":
			labels, valStr := splitLabelsAndValue(rest)
			val, err := parseMetricValue(valStr)
			if err != nil {
				continue
			}
			if strings.Contains(labels, `direction="in"`) {
				metrics.TrafficIn = val
			} else if strings.Contains(labels, `direction="out"`) {
				metrics.TrafficOut = val
			}
		}
	}

	return metrics
}

// splitMetricLine splits "metric_name{labels} value" into name and the remainder.
func splitMetricLine(line string) (string, string) {
	// Check for labels
	if idx := strings.IndexByte(line, '{'); idx != -1 {
		return line[:idx], line[idx:]
	}
	// No labels: "metric_name value [timestamp]"
	if idx := strings.IndexByte(line, ' '); idx != -1 {
		return line[:idx], line[idx:]
	}
	return line, ""
}

// splitLabelsAndValue splits "{labels} value" into labels portion and value string.
func splitLabelsAndValue(s string) (string, string) {
	if idx := strings.IndexByte(s, '}'); idx != -1 {
		return s[:idx+1], s[idx+1:]
	}
	return "", s
}

// parseMetricValue extracts the float64 value from a string like " 42.5" or " 42.5 1625000000000".
func parseMetricValue(s string) (float64, error) {
	s = strings.TrimSpace(s)
	// The value may be followed by an optional timestamp; take just the first token
	if idx := strings.IndexByte(s, ' '); idx != -1 {
		s = s[:idx]
	}
	return strconv.ParseFloat(s, 64)
}

// ============================================================
// Docker Discovery
// ============================================================

// discoverContainers finds all running containers that match the conduit image
// or have names starting with "conduit".
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

	result := make([]types.Container, 0, len(seen))
	for _, c := range seen {
		result = append(result, c)
	}
	return result, nil
}

// ============================================================
// Stats Collection
// ============================================================

// getContainerIP retrieves the first non-empty IP address from a container's networks.
func getContainerIP(ctx context.Context, cli *client.Client, containerID string) (string, error) {
	inspect, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspecting container %s: %w", containerID[:12], err)
	}

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

	return "", fmt.Errorf("no IP address found for container %s", containerID[:12])
}

// collectContainerStats gathers Docker stats for a single container.
func collectContainerStats(ctx context.Context, cli *client.Client, ctr types.Container, cfg *Config) ContainerInfo {
	name := ""
	if len(ctr.Names) > 0 {
		name = strings.TrimPrefix(ctr.Names[0], "/")
	}

	info := ContainerInfo{
		ID:     ctr.ID[:12],
		Name:   name,
		Status: ctr.State,
		Uptime: "0s",
	}

	// If the container isn't running, report it as down with zero stats
	if ctr.State != "running" {
		info.Status = "down"
		return info
	}

	// Compute uptime from container creation time
	info.Uptime = time.Since(time.Unix(ctr.Created, 0)).Truncate(time.Second).String()

	// Fetch one-shot container stats
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

// fetchAppMetrics queries the Prometheus endpoint inside a container and parses the response.
func fetchAppMetrics(containerIP string, cfg *Config) (*AppMetrics, error) {
	url := fmt.Sprintf("http://%s:%d%s", containerIP, cfg.MetricsPort, cfg.MetricsPath)

	httpClient := &http.Client{Timeout: cfg.MetricsTimeout}
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching metrics from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}

	return parsePrometheusMetrics(resp.Body), nil
}

// ============================================================
// Polling Engine
// ============================================================

// collectAll performs a full collection cycle: discover containers, collect stats
// and application metrics for each one.
func collectAll(ctx context.Context, cli *client.Client, cfg *Config) *StatusResponse {
	hostname, _ := os.Hostname()

	containers, err := discoverContainers(ctx, cli)
	if err != nil {
		log.Printf("WARN: container discovery failed: %v", err)
		return &StatusResponse{
			ServerID:        hostname,
			Timestamp:       time.Now().Unix(),
			TotalContainers: 0,
			Containers:      []ContainerInfo{},
		}
	}

	results := make([]ContainerInfo, len(containers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.MaxWorkers)

	for i, ctr := range containers {
		wg.Add(1)
		go func(idx int, c types.Container) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info := collectContainerStats(ctx, cli, c, cfg)

			// Only fetch app metrics if the container is running
			if info.Status == "running" {
				ip, err := getContainerIP(ctx, cli, c.ID)
				if err != nil {
					log.Printf("WARN: cannot get IP for %s: %v", info.Name, err)
				} else {
					appMetrics, err := fetchAppMetrics(ip, cfg)
					if err != nil {
						log.Printf("WARN: metrics unavailable for %s: %v", info.Name, err)
					} else {
						info.AppMetrics = appMetrics
					}
				}
			}

			results[idx] = info
		}(i, ctr)
	}
	wg.Wait()

	return &StatusResponse{
		ServerID:        hostname,
		Timestamp:       time.Now().Unix(),
		TotalContainers: len(results),
		Containers:      results,
	}
}

// pollLoop runs collectAll on a regular interval and updates the cache.
func pollLoop(ctx context.Context, cli *client.Client, cfg *Config, cache *StatusCache) {
	// Run immediately on startup
	cache.Set(collectAll(ctx, cli, cfg))
	log.Printf("Initial data collection complete (%d containers)", cache.Get().TotalContainers)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cache.Set(collectAll(ctx, cli, cfg))
		case <-ctx.Done():
			return
		}
	}
}

// ============================================================
// HTTP Server & Handlers
// ============================================================

// authMiddleware checks the X-Conduit-Auth header against the configured secret.
func authMiddleware(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Conduit-Auth")
		if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		next(w, r)
	}
}

// statusHandler returns the cached status response as JSON.
func statusHandler(cache *StatusCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := cache.Get()
		if resp == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"data not yet available"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// healthHandler provides a simple health check endpoint (no auth required).
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// ============================================================
// Main
// ============================================================

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg := loadConfig()

	if cfg.AuthSecret == "" {
		log.Fatal("CONDUIT_AUTH_SECRET environment variable is required")
	}

	// Initialize Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer cli.Close()

	// Verify Docker connectivity
	if _, err := cli.Ping(context.Background()); err != nil {
		log.Fatalf("Cannot reach Docker daemon: %v", err)
	}
	log.Println("Connected to Docker daemon")

	// Initialize cache and start background polling
	cache := &StatusCache{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pollLoop(ctx, cli, cfg, cache)

	// Set up HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/status", authMiddleware(cfg.AuthSecret, statusHandler(cache)))
	mux.HandleFunc("/health", healthHandler)

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("conduit-expose listening on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sig := <-sigChan
	log.Printf("Received %v, shutting down...", sig)

	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	log.Println("conduit-expose stopped")
}
