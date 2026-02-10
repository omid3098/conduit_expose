package main

import (
	"sync"
	"time"
)

// ============================================================
// App Metrics (from Docker container [STATS] log lines)
// ============================================================

// AppMetrics holds parsed metrics from a single conduit container's logs.
type AppMetrics struct {
	ConnectedClients  int64   `json:"connected_clients"`
	ConnectingClients int64   `json:"connecting_clients"`
	Announcing        int64   `json:"announcing"`
	IsLive            bool    `json:"is_live"`
	BytesUploaded     float64 `json:"bytes_uploaded"`
	BytesDownloaded   float64 `json:"bytes_downloaded"`
	UptimeSeconds     float64 `json:"uptime_seconds"`
	IdleSeconds       float64 `json:"idle_seconds"`
}

// ============================================================
// Container Settings (from settings.conf + Docker inspect)
// ============================================================

// ContainerSettings holds configuration from Conduit Manager's settings.conf
// and Docker inspect (auto_start from restart policy).
type ContainerSettings struct {
	MaxClients         int     `json:"max_clients"`
	BandwidthLimitMbps float64 `json:"bandwidth_limit_mbps"`
	AutoStart          bool    `json:"auto_start"`
	ContainerCount     int     `json:"container_count,omitempty"`
	SnowflakeEnabled   bool    `json:"snowflake_enabled,omitempty"`
	SnowflakeCount     int     `json:"snowflake_count,omitempty"`
}

// ============================================================
// Container Health (from Docker API + /proc)
// ============================================================

// ContainerHealth holds health indicators for a single container.
type ContainerHealth struct {
	RestartCount int  `json:"restart_count"`
	OOMKilled    bool `json:"oom_killed"`
	FDCount      int  `json:"fd_count"`
	ThreadCount  int  `json:"thread_count"`
}

// ============================================================
// System Metrics (host-level via /proc)
// ============================================================

// SystemMetrics holds host-level resource usage.
type SystemMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsedMB  float64 `json:"memory_used_mb"`
	MemoryTotalMB float64 `json:"memory_total_mb"`
	LoadAvg1m     float64 `json:"load_avg_1m"`
	LoadAvg5m     float64 `json:"load_avg_5m"`
	LoadAvg15m    float64 `json:"load_avg_15m"`
	DiskUsedGB    float64 `json:"disk_used_gb"`
	DiskTotalGB   float64 `json:"disk_total_gb"`
	NetInMbps     float64 `json:"net_in_mbps"`
	NetOutMbps    float64 `json:"net_out_mbps"`
	NetErrors     int64   `json:"net_errors"`
	NetDrops      int64   `json:"net_drops"`
}

// ============================================================
// Connection Tracking (TCP states from /proc/<pid>/net/tcp)
// ============================================================

// ConnectionStats holds aggregated TCP connection info across all containers.
type ConnectionStats struct {
	Total     int            `json:"total"`
	UniqueIPs int            `json:"unique_ips"`
	States    map[string]int `json:"states"`
}

// CountryStats holds client count for a single country (from Conduit Manager data).
type CountryStats struct {
	Country     string `json:"country"`
	Connections int    `json:"connections"`
}

// CountryTrafficStats holds bidirectional traffic totals for a single country.
type CountryTrafficStats struct {
	Country   string  `json:"country"`
	FromBytes float64 `json:"from_bytes"`
	ToBytes   float64 `json:"to_bytes"`
}

// ============================================================
// Conduit Manager Data (read from /opt/conduit/)
// ============================================================

// CMData holds all parsed data from Conduit Manager's data files.
type CMData struct {
	ClientsByCountry []CountryStats
	TrafficByCountry []CountryTrafficStats
	PeakConnections  int64
	TrackerStart     time.Time
	Settings         *CMSettings
	Available        bool
}

// CMSettings holds parsed values from Conduit Manager's settings.conf.
type CMSettings struct {
	MaxClients       int
	Bandwidth        float64
	ContainerCount   int
	SnowflakeEnabled bool
	SnowflakeCount   int
}

// ============================================================
// Session Aggregation
// ============================================================

// SessionInfo holds rolling aggregation data since last container restart.
type SessionInfo struct {
	StartTime          int64   `json:"start_time"`
	PeakConnections    int64   `json:"peak_connections"`
	AvgConnections     float64 `json:"avg_connections"`
	TotalUploadBytes   float64 `json:"total_upload_bytes"`
	TotalDownloadBytes float64 `json:"total_download_bytes"`
}

// ============================================================
// Snowflake Metrics
// ============================================================

// SnowflakeMetrics holds aggregated metrics from snowflake proxy containers.
type SnowflakeMetrics struct {
	TotalConnections int64   `json:"total_connections"`
	TimeoutsTotal    int64   `json:"timeouts_total"`
	InboundBytes     float64 `json:"inbound_bytes"`
	OutboundBytes    float64 `json:"outbound_bytes"`
}

// ============================================================
// Per-Container Info
// ============================================================

// ContainerInfo represents a single container's collected data.
type ContainerInfo struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Status     string             `json:"status"`
	CPUPercent float64            `json:"cpu_percent"`
	MemoryMB   float64            `json:"memory_mb"`
	Uptime     string             `json:"uptime"`
	Health     *ContainerHealth   `json:"health,omitempty"`
	AppMetrics *AppMetrics        `json:"app_metrics,omitempty"`
	Settings   *ContainerSettings `json:"settings,omitempty"`
}

// ============================================================
// Top-Level Response
// ============================================================

// StatusResponse is the top-level JSON response for GET /status.
type StatusResponse struct {
	ServerID          string              `json:"server_id"`
	Timestamp         int64               `json:"timestamp"`
	TotalContainers   int                 `json:"total_containers"`
	ConnectedClients  int64               `json:"connected_clients"`
	ConnectingClients int64               `json:"connecting_clients"`
	System            *SystemMetrics      `json:"system,omitempty"`
	Settings          *ContainerSettings  `json:"settings,omitempty"`
	Session           *SessionInfo        `json:"session,omitempty"`
	Connections       *ConnectionStats    `json:"connections,omitempty"`
	ClientsByCountry  []CountryStats      `json:"clients_by_country,omitempty"`
	TrafficByCountry  []CountryTrafficStats `json:"traffic_by_country,omitempty"`
	Snowflake         *SnowflakeMetrics   `json:"snowflake,omitempty"`
	Containers        []ContainerInfo     `json:"containers"`
	CMAvailable       bool                `json:"cm_available"`
}

// ============================================================
// Status Cache
// ============================================================

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
