package diagnostics

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckResolverPortAt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("checkResolverPortAt is darwin-only by contract")
	}

	tests := []struct {
		name          string
		fileContent   string // empty = no file
		clusterDomain string
		dnsListen     string
		wantWarn      bool
	}{
		{
			name:          "match: no warning",
			fileContent:   "nameserver 127.0.0.1\nport 11617\n",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      false,
		},
		{
			name:          "mismatch: warn",
			fileContent:   "nameserver 127.0.0.1\nport 5555\n",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      true,
		},
		{
			name:          "mismatch with extra lines: warn",
			fileContent:   "nameserver 127.0.0.1\nport 5555\nsearch_order 1\ntimeout 5\n",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      true,
		},
		{
			name:          "no file: silent",
			fileContent:   "",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      false,
		},
		{
			name:          "no port directive: silent",
			fileContent:   "nameserver 127.0.0.1\n",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      false,
		},
		{
			name:          "trailing dot in cluster domain handled",
			fileContent:   "nameserver 127.0.0.1\nport 5555\n",
			clusterDomain: "svc.cluster.local.",
			dnsListen:     "127.0.0.1:11617",
			wantWarn:      true,
		},
		{
			name:          "malformed dnsListen: silent",
			fileContent:   "nameserver 127.0.0.1\nport 5555\n",
			clusterDomain: "svc.cluster.local",
			dnsListen:     "not-a-listen-addr",
			wantWarn:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.fileContent != "" {
				path := filepath.Join(dir, strings.TrimSuffix(tt.clusterDomain, "."))
				if err := os.WriteFile(path, []byte(tt.fileContent), 0o644); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}
			got := checkResolverPortAt(dir, tt.clusterDomain, tt.dnsListen)
			if (got != "") != tt.wantWarn {
				t.Fatalf("warn?=%v (msg=%q)", got != "", got)
			}
			if tt.wantWarn {
				// Sanity: warning must mention both ports for actionability.
				if !strings.Contains(got, "5555") || !strings.Contains(got, "11617") {
					t.Errorf("warning missing ports: %q", got)
				}
			}
		})
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		in   string
		port int
		ok   bool
	}{
		{"127.0.0.1:11617", 11617, true},
		{":11617", 11617, true},
		{"[::1]:11617", 11617, true},
		{"", 0, false},
		{"127.0.0.1", 0, false},
		{"127.0.0.1:", 0, false},
		{"127.0.0.1:0", 0, false},
		{"127.0.0.1:-1", 0, false},
		{"127.0.0.1:abc", 0, false},
	}
	for _, tc := range cases {
		got, ok := portFromAddr(tc.in)
		if got != tc.port || ok != tc.ok {
			t.Errorf("portFromAddr(%q) = (%d,%v); want (%d,%v)", tc.in, got, ok, tc.port, tc.ok)
		}
	}
}
