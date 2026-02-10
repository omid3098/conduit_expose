package main

import (
	"log"
	"os"
	"time"
)

const (
	defaultListenAddr    = ":8081"
	defaultPollInterval  = 15 * time.Second
	defaultDockerTimeout = 5 * time.Second
	defaultMaxWorkers    = 10
	defaultHostProcPath  = "/host/proc"
	defaultHostRootPath      = "/host/root"
	defaultConduitInstallDir = "/opt/conduit"

	conduitImage = "ghcr.io/psiphon-inc/conduit/cli"
	conduitName  = "conduit"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	ListenAddr   string
	AuthSecret   string
	PollInterval time.Duration
	DockerTimeout time.Duration
	MaxWorkers   int
	HostProcPath      string
	HostRootPath      string
	ConduitInstallDir string
}

func loadConfig() *Config {
	return &Config{
		ListenAddr:   envOrDefault("CONDUIT_LISTEN_ADDR", defaultListenAddr),
		AuthSecret:   os.Getenv("CONDUIT_AUTH_SECRET"),
		PollInterval: envDurationOrDefault("CONDUIT_POLL_INTERVAL", defaultPollInterval),
		DockerTimeout: defaultDockerTimeout,
		MaxWorkers:   defaultMaxWorkers,
		HostProcPath: envOrDefault("CONDUIT_HOST_PROC", defaultHostProcPath),
		HostRootPath:      envOrDefault("CONDUIT_HOST_ROOT", defaultHostRootPath),
		ConduitInstallDir: envOrDefault("CONDUIT_INSTALL_DIR", defaultConduitInstallDir),
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// CMDataPath returns the full host-accessible path to the Conduit Manager install directory.
// e.g., "/host/root/opt/conduit"
func (c *Config) CMDataPath() string {
	return c.HostRootPath + c.ConduitInstallDir
}

// CMTrafficStatsPath returns the path to the traffic_stats directory.
// e.g., "/host/root/opt/conduit/traffic_stats"
func (c *Config) CMTrafficStatsPath() string {
	return c.CMDataPath() + "/traffic_stats"
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
