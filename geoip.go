package main

import (
	"log"
	"net"
	"os"
	"sync"

	"github.com/oschwald/geoip2-golang"
)

// GeoIPResolver resolves IP addresses to ISO country codes using a MaxMind GeoLite2 database.
type GeoIPResolver struct {
	db    *geoip2.Reader
	cache sync.Map // string(IP) → string(country code)
}

// NewGeoIPResolver loads the GeoLite2-Country database from the given path.
// Returns nil (not an error) if the database file does not exist, enabling graceful degradation.
func NewGeoIPResolver(dbPath string) *GeoIPResolver {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		log.Printf("WARN: GeoIP database not found at %s, country resolution disabled", dbPath)
		return nil
	}

	db, err := geoip2.Open(dbPath)
	if err != nil {
		log.Printf("WARN: failed to open GeoIP database: %v, country resolution disabled", err)
		return nil
	}

	log.Printf("GeoIP database loaded from %s", dbPath)
	return &GeoIPResolver{db: db}
}

// Lookup returns the ISO country code for the given IP, or "XX" if unknown.
func (g *GeoIPResolver) Lookup(ip net.IP) string {
	if g == nil {
		return ""
	}

	key := ip.String()

	// Check cache first
	if cached, ok := g.cache.Load(key); ok {
		return cached.(string)
	}

	record, err := g.db.Country(ip)
	if err != nil || record.Country.IsoCode == "" {
		g.cache.Store(key, "XX")
		return "XX"
	}

	g.cache.Store(key, record.Country.IsoCode)
	return record.Country.IsoCode
}

// Close releases the GeoIP database resources.
func (g *GeoIPResolver) Close() {
	if g != nil && g.db != nil {
		g.db.Close()
	}
}
