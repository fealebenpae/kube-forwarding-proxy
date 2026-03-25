package app

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Config holds all configuration for the proxy.
type Config struct {
	// Interface is the network interface name (e.g. "eth0", "lo0") or IP address
	// (e.g. "192.168.1.10") to which VIPs are added.
	// Defaults to the platform loopback interface.
	Interface string

	// VIPCIDR is the CIDR range for virtual IP allocation (e.g. "127.0.0.0/8").
	// Defaults to the loopback CIDR.
	VIPCIDR string

	// ClusterDomain is the K8s cluster DNS suffix (e.g. "svc.cluster.local").
	ClusterDomain string

	// LogLevel controls logging verbosity (debug, info, warn, error).
	LogLevel string

	// HTTPListen is the address the HTTP server binds to (e.g. "127.0.0.1:8080").
	// Defaults to "127.0.0.1:8080".
	HTTPListen string

	// DNSListen is the address the DNS server binds to (e.g. "127.0.0.1:53").
	// Defaults to "127.0.0.1:0" (random port on loopback interface).
	DNSListen string

	// SOCKSListen is the address the SOCKS5 proxy binds to (e.g. "127.0.0.1:1080").
	// Defaults to "127.0.0.1:0" (random port on loopback interface).
	SOCKSListen string

	// VIPIdleTimeout is how long a VIP and its port-forward TCP listeners are
	// kept alive after the last active connection closes and no DNS queries
	// have refreshed it. Zero (the default) disables idle expiry entirely.
	VIPIdleTimeout time.Duration
}

// NewConfigFromEnvironment reads configuration from environment variables with sensible defaults.
func NewConfigFromEnvironment() (Config, error) {
	cfg := Config{
		Interface:     getEnv("INTERFACE", "127.0.0.1"),
		VIPCIDR:       getEnv("VIP_CIDR", "127.0.0.0/8"),
		ClusterDomain: getEnv("CLUSTER_DOMAIN", "svc.cluster.local"),
		LogLevel:      strings.ToLower(getEnv("LOG_LEVEL", "info")),
		HTTPListen:    getEnv("HTTP_LISTEN", "127.0.0.1:8080"),
		DNSListen:     getEnv("DNS_LISTEN", "127.0.0.1:0"),
		SOCKSListen:   getEnv("SOCKS_LISTEN", "127.0.0.1:0"),
	}

	if s := os.Getenv("VIP_IDLE_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return Config{}, fmt.Errorf("VIP_IDLE_TIMEOUT: %w", err)
		}
		cfg.VIPIdleTimeout = d
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the configuration is well-formed.
func (c Config) Validate() error {
	if c.Interface == "" {
		return fmt.Errorf("INTERFACE must not be empty: set it to an interface name (e.g. eth0) or an IP address")
	}

	_, _, err := net.ParseCIDR(c.VIPCIDR)
	if err != nil {
		return fmt.Errorf("VIP_CIDR is not a valid CIDR: %w", err)
	}

	if c.ClusterDomain == "" {
		return fmt.Errorf("CLUSTER_DOMAIN must not be empty")
	}

	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.LogLevel] {
		return fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error; got %q", c.LogLevel)
	}

	if c.DNSListen == "" {
		return fmt.Errorf("DNS_LISTEN must not be empty")
	}

	if c.SOCKSListen == "" {
		return fmt.Errorf("SOCKS_LISTEN must not be empty")
	}

	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
