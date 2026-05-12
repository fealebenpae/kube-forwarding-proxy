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
	// Interface is the network interface name (e.g. "eth0", "lo0") or IP
	// address (e.g. "192.168.1.10") to which VIPs are added. Defaults to "lo0".
	Interface string

	// VIPCIDR is the CIDR range for virtual IP allocation. Defaults to
	// "127.50.0.0/24" — the same /24 `k8s-service-proxy install` pre-aliases.
	VIPCIDR string

	// ClusterDomain is the K8s service DNS suffix. Hardcoded to
	// "svc.cluster.local" — the universal Kubernetes default. We intentionally
	// don't read a CLUSTER_DOMAIN env var because the same name is used by
	// downstream toolchains (e.g. kind / Helm) to mean the cluster's *base*
	// domain ("cluster.local") rather than the service suffix kfp needs;
	// inheriting that value silently breaks DNS name parsing.
	ClusterDomain string

	// LogLevel controls logging verbosity (debug, info, warn, error).
	LogLevel string

	// HTTPListen is the address the HTTP server binds to.
	// Defaults to "127.0.0.1:11616" (the kfp control port).
	HTTPListen string

	// DNSListen is the address the DNS server binds to. Defaults to
	// "127.0.0.1:11617" — the same port `k8s-service-proxy install` writes
	// into /etc/resolver/<cluster-domain>.
	DNSListen string

	// SOCKSListen is the address the SOCKS5 proxy binds to.
	// Defaults to "127.0.0.1:11618".
	SOCKSListen string

	// VIPIdleTimeout is how long a VIP and its port-forward TCP listeners are
	// kept alive after the last active connection closes and no DNS queries
	// have refreshed it. Zero (the default) disables idle expiry entirely.
	VIPIdleTimeout time.Duration

	// VIPAliasMode controls how VIPs are bound to the configured interface.
	//
	//	"auto"          — call ifconfig/ip alias for every VIP (default; needs root on macOS).
	//	"preallocated"  — assume the VIPs are already aliased to the interface;
	//	                  verify presence and never call ifconfig. Used by the macOS
	//	                  installer to keep daily operation unprivileged.
	VIPAliasMode string
}

// NewConfigFromEnvironment reads configuration from environment variables.
//
// Defaults align with what `k8s-service-proxy install` configures so the
// daemon runs without any env vars on a freshly-installed macOS host. Linux
// container deployments (e.g. the docker-compose sidecar) set these
// explicitly; see the README for the override list (VIP_ALIAS_MODE=auto,
// VIP_CIDR/INTERFACE for the bridge subnet, DNS_LISTEN=:53).
func NewConfigFromEnvironment() (Config, error) {
	cfg := Config{
		Interface:     getEnv("INTERFACE", "lo0"),
		VIPCIDR:       getEnv("VIP_CIDR", "127.50.0.0/24"),
		ClusterDomain: "svc.cluster.local",
		LogLevel:      strings.ToLower(getEnv("LOG_LEVEL", "info")),
		HTTPListen:    getEnv("HTTP_LISTEN", "127.0.0.1:11616"),
		DNSListen:     getEnv("DNS_LISTEN", "127.0.0.1:11617"),
		SOCKSListen:   getEnv("SOCKS_LISTEN", "127.0.0.1:11618"),
		VIPAliasMode:  strings.ToLower(getEnv("VIP_ALIAS_MODE", "preallocated")),
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

	switch c.VIPAliasMode {
	case "", "auto", "preallocated":
	default:
		return fmt.Errorf("VIP_ALIAS_MODE must be one of auto, preallocated; got %q", c.VIPAliasMode)
	}

	return nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
