package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/proxy"
)

func TestSOCKS5_StartShutdown(t *testing.T) {
	p, err := NewSOCKS5Proxy("127.0.0.1:0", "svc.cluster.local", nil, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewSOCKS5Proxy: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := p.listener.Addr().String()

	// Verify we can connect.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial SOCKS5: %v", err)
	}
	_ = conn.Close()

	p.Shutdown()

	// Verify the listener is closed.
	_, err = net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected dial to fail after shutdown")
	}
}

func TestSOCKS5_DirectDial(t *testing.T) {
	// Start an echo server.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer func() { _ = echoLn.Close() }()

	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	// Start SOCKS5 proxy.
	p, err := NewSOCKS5Proxy("127.0.0.1:0", "svc.cluster.local", nil, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewSOCKS5Proxy: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Shutdown()

	// Connect through SOCKS5.
	dialer, err := proxy.SOCKS5("tcp", p.listener.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}

	conn, err := dialer.Dial("tcp", echoLn.Addr().String())
	if err != nil {
		t.Fatalf("SOCKS5 dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	msg := []byte("hello socks5")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(buf) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, buf)
	}
}

func TestParseClusterHost(t *testing.T) {
	tests := []struct {
		host    string
		domain  string
		wantPod string
		wantSvc string
		wantNS  string
		wantOK  bool
	}{
		// regular service FQDNs
		{"my-svc.default.svc.cluster.local", "svc.cluster.local", "", "my-svc", "default", true},
		{"api.kube-system.svc.cluster.local", "svc.cluster.local", "", "api", "kube-system", true},
		// headless pod FQDNs
		{"pod-0.my-svc.default.svc.cluster.local", "svc.cluster.local", "pod-0", "my-svc", "default", true},
		{"redis-1.cache.prod.svc.cluster.local", "svc.cluster.local", "redis-1", "cache", "prod", true},
		// invalid
		{"just-name.svc.cluster.local", "svc.cluster.local", "", "", "", false}, // missing namespace
		{"google.com", "svc.cluster.local", "", "", "", false},
		{"", "svc.cluster.local", "", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			pod, svc, ns, _, ok := ParseClusterHost(tt.host, tt.domain)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if pod != tt.wantPod {
				t.Fatalf("pod=%q, want %q", pod, tt.wantPod)
			}
			if svc != tt.wantSvc {
				t.Fatalf("svc=%q, want %q", svc, tt.wantSvc)
			}
			if ns != tt.wantNS {
				t.Fatalf("ns=%q, want %q", ns, tt.wantNS)
			}
		})
	}
}

func TestIsClusterHost(t *testing.T) {
	p := &SOCKS5Proxy{clusterDomain: "svc.cluster.local"}

	tests := []struct {
		host string
		want bool
	}{
		{"my-svc.default.svc.cluster.local", true},
		{"my-svc.default.svc.cluster.local.", true},
		{"google.com", false},
		{"svc.cluster.local", false}, // bare domain, no service prefix
		{"", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.host), func(t *testing.T) {
			got := IsClusterHost(tt.host, p.clusterDomain)
			if got != tt.want {
				t.Fatalf("isClusterHost(%q)=%v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestSOCKS5_DialClusterInvalidPort(t *testing.T) {
	p := &SOCKS5Proxy{
		clusterDomain: "svc.cluster.local",
	}

	ctx := context.Background()
	_, err := p.dialCluster(ctx, "my-svc.default.svc.cluster.local", "notanumber")
	if err == nil {
		t.Fatal("expected error for non-numeric port")
	}
}

func TestSOCKS5_DialClusterInvalidPort_HeadlessPod(t *testing.T) {
	p := &SOCKS5Proxy{
		clusterDomain: "svc.cluster.local",
	}

	ctx := context.Background()
	_, err := p.dialCluster(ctx, "pod-0.my-svc.default.svc.cluster.local", "notanumber")
	if err == nil {
		t.Fatal("expected error for non-numeric port on headless pod FQDN")
	}
}
