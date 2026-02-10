package main

import (
	"net"

	"github.com/phuslu/iploc"
)

// GeoIPResolver resolves IP addresses to ISO country codes using embedded IP data.
// Uses github.com/phuslu/iploc which bundles IP-to-country mappings directly in the binary —
// no external database files, license keys, or downloads required.
type GeoIPResolver struct{}

// NewGeoIPResolver creates a resolver. Always succeeds since data is embedded.
func NewGeoIPResolver() *GeoIPResolver {
	return &GeoIPResolver{}
}

// Lookup returns the ISO country code for the given IP (e.g. "IR", "CN"),
// or "XX" if unknown.
func (g *GeoIPResolver) Lookup(ip net.IP) string {
	if g == nil {
		return ""
	}

	country := iploc.Country(ip)
	if country == "" {
		return "XX"
	}
	return country
}
