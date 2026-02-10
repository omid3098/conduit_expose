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

	// Initialize GeoIP resolver (embedded data, always available)
	geo := NewGeoIPResolver()

	// Initialize session tracker
	session := NewSessionTracker()

	// Initialize cache and start background polling
	cache := &StatusCache{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pollLoop(ctx, cli, cfg, cache, geo, session)

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

func pollLoop(ctx context.Context, cli *client.Client, cfg *Config, cache *StatusCache, geo *GeoIPResolver, session *SessionTracker) {
	cache.Set(collectAll(ctx, cli, cfg, geo, session))
	log.Printf("Initial data collection complete (%d containers)", cache.Get().TotalContainers)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cache.Set(collectAll(ctx, cli, cfg, geo, session))
		case <-ctx.Done():
			return
		}
	}
}

// collectAll performs a full collection cycle.
func collectAll(ctx context.Context, cli *client.Client, cfg *Config, geo *GeoIPResolver, session *SessionTracker) *StatusResponse {
	hostname, _ := os.Hostname()

	// 1. System-level metrics
	systemMetrics := collectSystemMetrics(cfg)

	// 2. Discover containers (with self-filtering)
	containers, err := discoverContainers(ctx, cli)
	if err != nil {
		log.Printf("WARN: container discovery failed: %v", err)
		return &StatusResponse{
			ServerID:        hostname,
			Timestamp:       time.Now().Unix(),
			TotalContainers: 0,
			System:          systemMetrics,
			Containers:      []ContainerInfo{},
		}
	}

	// 3. Parallel per-container collection
	type perContainerResult struct {
		info     ContainerInfo
		connStat *ConnectionStats
		countries []CountryStats
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
			var countries []CountryStats

			if info.Status == "running" {
				// Inspect container (needed for IP, health, PID)
				inspect, inspectErr := cli.ContainerInspect(ctx, c.ID)
				if inspectErr != nil {
					log.Printf("WARN: cannot inspect %s: %v", info.Name, inspectErr)
				} else {
					// App metrics + settings from Prometheus
					ip, ipErr := getContainerIP(ctx, cli, inspect)
					if ipErr != nil {
						log.Printf("WARN: cannot get IP for %s: %v", info.Name, ipErr)
					} else {
						appMetrics, settings, metricsErr := fetchAppMetrics(ip, cfg)
						if metricsErr != nil {
							log.Printf("WARN: metrics unavailable for %s: %v", info.Name, metricsErr)
						} else {
							info.AppMetrics = appMetrics
							if settings != nil {
								settings.AutoStart = extractAutoStart(inspect)
								info.Settings = settings
							}
						}
					}

					// Container health from Docker inspect + /proc
					info.Health = collectContainerHealth(inspect, cfg.HostProcPath)

					// TCP connections from /proc/<pid>/net/tcp
					if inspect.State != nil && inspect.State.Pid > 0 {
						connStat, countries = collectContainerConnections(cfg.HostProcPath, inspect.State.Pid, geo)
					}
				}
			}

			results[idx] = perContainerResult{
				info:      info,
				connStat:  connStat,
				countries: countries,
			}
		}(i, ctr)
	}
	wg.Wait()

	// 4. Aggregate results
	containerInfos := make([]ContainerInfo, len(results))
	var allConnStats []*ConnectionStats
	var allCountries [][]CountryStats
	var totalConnected int64
	var totalUpload, totalDownload float64
	var aggSettings *ContainerSettings

	for i, r := range results {
		containerInfos[i] = r.info

		if r.connStat != nil {
			allConnStats = append(allConnStats, r.connStat)
		}
		if r.countries != nil {
			allCountries = append(allCountries, r.countries)
		}

		if r.info.AppMetrics != nil {
			totalConnected += r.info.AppMetrics.ConnectedClients
			totalUpload += r.info.AppMetrics.BytesUploaded
			totalDownload += r.info.AppMetrics.BytesDownloaded
		}

		// Aggregate settings: take max of MaxClients and BandwidthLimitMbps, OR of AutoStart
		if r.info.Settings != nil {
			if aggSettings == nil {
				s := *r.info.Settings
				aggSettings = &s
			} else {
				if r.info.Settings.MaxClients > aggSettings.MaxClients {
					aggSettings.MaxClients = r.info.Settings.MaxClients
				}
				if r.info.Settings.BandwidthLimitMbps > aggSettings.BandwidthLimitMbps {
					aggSettings.BandwidthLimitMbps = r.info.Settings.BandwidthLimitMbps
				}
				if r.info.Settings.AutoStart {
					aggSettings.AutoStart = true
				}
			}
		}
	}

	// Merge connections and countries across containers
	mergedConns := mergeConnectionStats(allConnStats)
	if mergedConns.Total == 0 {
		mergedConns = nil
	}
	mergedCountries := mergeCountryStats(allCountries)

	// Update session tracker
	session.Update(totalConnected, totalUpload, totalDownload)

	return &StatusResponse{
		ServerID:         hostname,
		Timestamp:        time.Now().Unix(),
		TotalContainers:  len(containerInfos),
		System:           systemMetrics,
		Settings:         aggSettings,
		Session:          session.Snapshot(),
		Connections:      mergedConns,
		ClientsByCountry: mergedCountries,
		Containers:       containerInfos,
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
