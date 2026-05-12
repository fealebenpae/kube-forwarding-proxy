package app

import (
	"os"
	"testing"
)

// clearEnv unsets all env vars that Load() reads for the duration of t,
// restoring their previous values on cleanup.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"INTERFACE", "VIP_CIDR", "LOG_LEVEL", "HTTP_LISTEN", "DNS_LISTEN", "SOCKS_LISTEN", "VIP_ALIAS_MODE", "VIP_IDLE_TIMEOUT"} {
		if prev, had := os.LookupEnv(k); had {
			_ = os.Unsetenv(k)
			k, prev := k, prev
			t.Cleanup(func() { _ = os.Setenv(k, prev) })
		}
	}
}

func TestNewConfigFromEnvironment_Defaults(t *testing.T) {
	clearEnv(t)

	cfg, err := NewConfigFromEnvironment()
	if err != nil {
		t.Fatalf("NewConfigFromEnvironment() with defaults failed: %v", err)
	}
	if cfg.Interface != "lo0" {
		t.Errorf("Interface = %q, want lo0", cfg.Interface)
	}
	if cfg.VIPCIDR != "127.50.0.0/24" {
		t.Errorf("VIPCIDR = %q, want 127.50.0.0/24", cfg.VIPCIDR)
	}
	if cfg.ClusterDomain != "svc.cluster.local" {
		t.Errorf("ClusterDomain = %q, want svc.cluster.local", cfg.ClusterDomain)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.HTTPListen != "127.0.0.1:11616" {
		t.Errorf("HTTPListen = %q, want 127.0.0.1:11616", cfg.HTTPListen)
	}
	if cfg.DNSListen != "127.0.0.1:11617" {
		t.Errorf("DNSListen = %q, want 127.0.0.1:11617", cfg.DNSListen)
	}
	if cfg.SOCKSListen != "127.0.0.1:11618" {
		t.Errorf("SOCKSListen = %q, want 127.0.0.1:11618", cfg.SOCKSListen)
	}
	if cfg.VIPAliasMode != "preallocated" {
		t.Errorf("VIPAliasMode = %q, want preallocated", cfg.VIPAliasMode)
	}
}

func TestNewConfigFromEnvironment_FromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("INTERFACE", "eth0")
	t.Setenv("VIP_CIDR", "10.99.0.0/16")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("HTTP_LISTEN", ":9090")
	t.Setenv("DNS_LISTEN", ":5353")
	t.Setenv("SOCKS_LISTEN", ":1080")

	cfg, err := NewConfigFromEnvironment()
	if err != nil {
		t.Fatalf("NewConfigFromEnvironment() from env failed: %v", err)
	}
	if cfg.Interface != "eth0" {
		t.Errorf("Interface = %q, want eth0", cfg.Interface)
	}
	if cfg.VIPCIDR != "10.99.0.0/16" {
		t.Errorf("VIPCIDR = %q, want 10.99.0.0/16", cfg.VIPCIDR)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.DNSListen != ":5353" {
		t.Errorf("DNSListen = %q, want :5353", cfg.DNSListen)
	}
	if cfg.SOCKSListen != ":1080" {
		t.Errorf("SOCKSListen = %q, want :1080", cfg.SOCKSListen)
	}
}

// ClusterDomain is no longer read from CLUSTER_DOMAIN env; verify the
// var is ignored even when set to a conflicting value.
func TestNewConfigFromEnvironment_IgnoresClusterDomainEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("CLUSTER_DOMAIN", "cluster.local")
	cfg, err := NewConfigFromEnvironment()
	if err != nil {
		t.Fatalf("NewConfigFromEnvironment() failed: %v", err)
	}
	if cfg.ClusterDomain != "svc.cluster.local" {
		t.Errorf("ClusterDomain = %q, want svc.cluster.local (env should be ignored)", cfg.ClusterDomain)
	}
}

func TestValidate_InvalidCIDR(t *testing.T) {
	cfg := Config{
		Interface:     "eth0",
		VIPCIDR:       "not-a-cidr",
		ClusterDomain: "svc.cluster.local",
		LogLevel:      "info",
		HTTPListen:    ":8080",
		DNSListen:     ":0",
		SOCKSListen:   ":0",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid CIDR, got nil")
	}
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	cfg := Config{
		Interface:     "eth0",
		VIPCIDR:       "127.0.0.0/8",
		ClusterDomain: "svc.cluster.local",
		LogLevel:      "verbose",
		HTTPListen:    ":8080",
		DNSListen:     ":0",
		SOCKSListen:   ":0",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid log level, got nil")
	}
}

func TestValidate_EmptyFields(t *testing.T) {
	base := Config{
		Interface:     "eth0",
		VIPCIDR:       "127.0.0.0/8",
		ClusterDomain: "svc.cluster.local",
		LogLevel:      "info",
		HTTPListen:    ":8080",
		DNSListen:     ":0",
		SOCKSListen:   ":0",
	}
	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty Interface", func() Config { c := base; c.Interface = ""; return c }()},
		{"empty DNSListen", func() Config { c := base; c.DNSListen = ""; return c }()},
		{"empty SOCKSListen", func() Config { c := base; c.SOCKSListen = ""; return c }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}
