package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

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

	if _, err := cli.Ping(context.Background()); err != nil {
		log.Fatalf("Cannot reach Docker daemon: %v", err)
	}
	log.Println("Connected to Docker daemon")

	// Initialize session tracker
	session := NewSessionTracker()

	// Initialize cache and start background polling
	cache := &StatusCache{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pollLoop(ctx, cli, cfg, cache, session)

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

// ============================================================
// Polling Engine
// ============================================================

func pollLoop(ctx context.Context, cli *client.Client, cfg *Config, cache *StatusCache, session *SessionTracker) {
	cache.Set(collectAll(ctx, cli, cfg, session))
	log.Printf("Initial data collection complete (%d containers)", cache.Get().TotalContainers)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cache.Set(collectAll(ctx, cli, cfg, session))
		case <-ctx.Done():
			return
		}
	}
}

// collectAll performs a full collection cycle.
func collectAll(ctx context.Context, cli *client.Client, cfg *Config, session *SessionTracker) *StatusResponse {
	hostname, _ := os.Hostname()

	// 1. System-level metrics
	systemMetrics := collectSystemMetrics(cfg)

	// 2. Read Conduit Manager data files (country, traffic, settings, peak)
	cmData := ReadCMData(cfg)
	if !cmData.Available {
		log.Println("WARN: Conduit Manager data not available at", cfg.CMDataPath())
	}

	// 3. Discover containers (with self-filtering)
	containers, err := discoverContainers(ctx, cli)
	if err != nil {
		log.Printf("WARN: container discovery failed: %v", err)
		return &StatusResponse{
			ServerID:        hostname,
			Timestamp:       time.Now().Unix(),
			TotalContainers: 0,
			System:          systemMetrics,
			Containers:      []ContainerInfo{},
			CMAvailable:     cmData.Available,
		}
	}

	// 4. Parallel per-container collection
	type perContainerResult struct {
		info      ContainerInfo
		connStat  *ConnectionStats
		autoStart bool
	}

	results := make([]perContainerResult, len(containers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.MaxWorkers)

	for i, ctr := range containers {
		wg.Add(1)
		go func(idx int, c types.Container) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info := collectContainerStats(ctx, cli, c, cfg)
			var connStat *ConnectionStats
			var autoStart bool

			if info.Status == "running" {
				inspect, inspectErr := cli.ContainerInspect(ctx, c.ID)
				if inspectErr != nil {
					log.Printf("WARN: cannot inspect %s: %v", info.Name, inspectErr)
				} else {
					// App metrics from container logs ([STATS] lines)
					appMetrics, metricsErr := fetchAppMetricsFromLogs(ctx, cli, c.ID, cfg)
					if metricsErr != nil {
						log.Printf("WARN: logs unavailable for %s: %v", info.Name, metricsErr)
					} else if appMetrics != nil {
						appMetrics.UptimeSeconds = containerUptimeSeconds(inspect)
						info.AppMetrics = appMetrics
					}

					// AutoStart from Docker restart policy
					autoStart = extractAutoStart(inspect)

					// Container health from Docker inspect + /proc
					info.Health = collectContainerHealth(inspect, cfg.HostProcPath)

					// TCP connection states from /proc/<pid>/net/tcp
					if inspect.State != nil && inspect.State.Pid > 0 {
						connStat = collectContainerConnections(cfg.HostProcPath, inspect.State.Pid)
					}
				}
			}

			results[idx] = perContainerResult{
				info:      info,
				connStat:  connStat,
				autoStart: autoStart,
			}
		}(i, ctr)
	}
	wg.Wait()

	// 5. Aggregate results
	containerInfos := make([]ContainerInfo, len(results))
	var allConnStats []*ConnectionStats
	var totalConnected int64
	var totalConnecting int64
	var totalUpload, totalDownload float64
	var maxUptimeSeconds float64
	var anyAutoStart bool

	for i, r := range results {
		containerInfos[i] = r.info

		if r.connStat != nil {
			allConnStats = append(allConnStats, r.connStat)
		}

		if r.info.AppMetrics != nil {
			totalConnected += r.info.AppMetrics.ConnectedClients
			totalConnecting += r.info.AppMetrics.ConnectingClients
			totalUpload += r.info.AppMetrics.BytesUploaded
			totalDownload += r.info.AppMetrics.BytesDownloaded
			if r.info.AppMetrics.UptimeSeconds > maxUptimeSeconds {
				maxUptimeSeconds = r.info.AppMetrics.UptimeSeconds
			}
		}

		if r.autoStart {
			anyAutoStart = true
		}
	}

	// Merge TCP connection stats across containers
	mergedConns := mergeConnectionStats(allConnStats)
	if mergedConns.Total == 0 {
		mergedConns = nil
	}

	// 6. Build settings from CM data + Docker auto-start
	var aggSettings *ContainerSettings
	if cmData.Available && cmData.Settings != nil {
		aggSettings = &ContainerSettings{
			MaxClients:         cmData.Settings.MaxClients,
			BandwidthLimitMbps: cmData.Settings.Bandwidth,
			AutoStart:          anyAutoStart,
			ContainerCount:     cmData.Settings.ContainerCount,
			SnowflakeEnabled:   cmData.Settings.SnowflakeEnabled,
			SnowflakeCount:     cmData.Settings.SnowflakeCount,
		}
	} else if anyAutoStart {
		aggSettings = &ContainerSettings{AutoStart: true}
	}

	// 7. Country data from CM files
	// CM's tracker_snapshot contains unique IPs from the latest tcpdump capture window.
	// These raw counts include scanners, system traffic, etc. — more than actual tunnel
	// sessions. Conduit Manager scales them proportionally:
	//   est = (country_ips * connected_clients) / total_snapshot_ips
	// This makes country totals sum to exactly connected_clients.
	var clientsByCountry []CountryStats
	var trafficByCountry []CountryTrafficStats
	if cmData.Available {
		clientsByCountry = scaleCountryStats(cmData.ClientsByCountry, totalConnected)
		trafficByCountry = cmData.TrafficByCountry
	}

	// 8. Update session tracker
	session.Update(totalConnected, totalUpload, totalDownload, maxUptimeSeconds)
	if cmData.Available {
		session.UpdateFromCM(cmData.PeakConnections, cmData.TrackerStart)
	}

	// 9. Collect snowflake metrics if enabled
	var snowflake *SnowflakeMetrics
	if cmData.Available && cmData.Settings != nil && cmData.Settings.SnowflakeEnabled {
		snowflake = collectSnowflakeMetrics(ctx, cfg)
	}

	return &StatusResponse{
		ServerID:          hostname,
		Timestamp:         time.Now().Unix(),
		TotalContainers:   len(containerInfos),
		ConnectedClients:  totalConnected,
		ConnectingClients: totalConnecting,
		System:            systemMetrics,
		Settings:          aggSettings,
		Session:           session.Snapshot(),
		Connections:       mergedConns,
		ClientsByCountry:  clientsByCountry,
		TrafficByCountry:  trafficByCountry,
		Snowflake:         snowflake,
		Containers:        containerInfos,
		CMAvailable:       cmData.Available,
	}
}

// ============================================================
// HTTP Handlers
// ============================================================

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

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}
