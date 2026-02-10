package main

import (
	"sync"
	"time"
)

// SessionTracker maintains rolling aggregation state across poll cycles.
// It tracks peak/average connections and cumulative traffic since the last
// detected container restart (counter reset).
type SessionTracker struct {
	mu           sync.Mutex
	startTime    time.Time
	peakConns    int64
	sampleCount  int64
	connSum      int64
	lastUpload   float64
	lastDownload float64
}

// NewSessionTracker creates a session tracker starting now.
func NewSessionTracker() *SessionTracker {
	return &SessionTracker{
		startTime: time.Now(),
	}
}

// Update records a new sample. If cumulative traffic drops (counter reset),
// the session is reset automatically.
func (s *SessionTracker) Update(totalConnected int64, totalUpload, totalDownload float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Detect counter reset (container restart)
	if totalUpload < s.lastUpload || totalDownload < s.lastDownload {
		s.startTime = time.Now()
		s.peakConns = 0
		s.sampleCount = 0
		s.connSum = 0
	}

	if totalConnected > s.peakConns {
		s.peakConns = totalConnected
	}

	s.sampleCount++
	s.connSum += totalConnected
	s.lastUpload = totalUpload
	s.lastDownload = totalDownload
}

// Snapshot returns the current session info.
func (s *SessionTracker) Snapshot() *SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	var avg float64
	if s.sampleCount > 0 {
		avg = float64(s.connSum) / float64(s.sampleCount)
	}

	return &SessionInfo{
		StartTime:          s.startTime.Unix(),
		PeakConnections:    s.peakConns,
		AvgConnections:     avg,
		TotalUploadBytes:   s.lastUpload,
		TotalDownloadBytes: s.lastDownload,
	}
}
