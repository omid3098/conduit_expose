package main

import (
	"log"
	"os"
	"strconv"
	"time"
)

const (
	defaultListenAddr     = ":8081"
	defaultMetricsPort    = 9090
	defaultMetricsPath    = "/metrics"
	defaultPollInterval   = 15 * time.Second
	defaultDockerTimeout  = 5 * time.Second
	defaultMetricsTimeout = 3 * time.Second
	defaultMaxWorkers     = 10
	defaultHostProcPath   = "/host/proc"
	defaultHostRootPath   = "/host/root"
	defaultGeoIPPath      = "/data/GeoLite2-Country.mmdb"

	conduitImage = "ghcr.io/psiphon-inc/conduit/cli"
	conduitName  = "conduit"
)

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
	HostProcPath   string
	HostRootPath   string
	GeoIPPath      string
}

func loadConfig() *Config {
	return &Config{
		ListenAddr:     envOrDefault("CONDUIT_LISTEN_ADDR", defaultListenAddr),
		AuthSecret:     os.Getenv("CONDUIT_AUTH_SECRET"),
		MetricsPort:    envIntOrDefault("CONDUIT_METRICS_PORT", defaultMetricsPort),
		MetricsPath:    envOrDefault("CONDUIT_METRICS_PATH", defaultMetricsPath),
		PollInterval:   envDurationOrDefault("CONDUIT_POLL_INTERVAL", defaultPollInterval),
		DockerTimeout:  defaultDockerTimeout,
		MetricsTimeout: defaultMetricsTimeout,
		MaxWorkers:     defaultMaxWorkers,
		HostProcPath:   envOrDefault("CONDUIT_HOST_PROC", defaultHostProcPath),
		HostRootPath:   envOrDefault("CONDUIT_HOST_ROOT", defaultHostRootPath),
		GeoIPPath:      envOrDefault("CONDUIT_GEOIP_PATH", defaultGeoIPPath),
	}
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
