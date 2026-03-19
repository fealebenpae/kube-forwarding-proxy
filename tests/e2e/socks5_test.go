package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestSOCKS5_ClusterIP verifies that the SOCKS5 proxy tunnels traffic to a
// ClusterIP service and returns a valid response.
func TestSOCKS5_ClusterIP(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-socks-cip", "nginx")

	body := httpGetViaSOCKS5(t, srv.SOCKSAddr, "http://nginx-clusterip.default.svc.cluster.local/")
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response, got: %s", body)
	}
	t.Logf("ClusterIP via SOCKS5: OK")
}

// TestSOCKS5_HeadlessPod verifies that the SOCKS5 proxy resolves pod-specific
// headless FQDNs and tunnels traffic to the correct pod.
func TestSOCKS5_HeadlessPod(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-socks-hl", "nginx")

	body := httpGetViaSOCKS5(t, srv.SOCKSAddr, "http://nginx-0.nginx-headless.default.svc.cluster.local/")
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response, got: %s", body)
	}
	t.Logf("Headless pod via SOCKS5: OK")
}

// TestSOCKS5_MultiPort deploys two nginx containers in a single pod — one
// serving on port 80 and one on port 8080 — behind a single ClusterIP service
// that exposes both ports. It verifies that both ports are reachable via SOCKS5.
func TestSOCKS5_MultiPort(t *testing.T) {
	srv := startProxy(t)
	setupSingleClusterMultiPort(t, srv, "e2e-socks-mp", "nginx-mp")

	for _, tc := range []struct {
		port int
		name string
	}{
		{80, "port 80"},
		{8080, "port 8080"},
	} {
		url := fmt.Sprintf("http://nginx-mp-clusterip.default.svc.cluster.local:%d/", tc.port)
		body := httpGetViaSOCKS5(t, srv.SOCKSAddr, url)
		if !strings.Contains(body, "nginx") {
			t.Fatalf("expected nginx response on %s, got: %s", tc.name, body)
		}
		t.Logf("MultiPort %s via SOCKS5: OK", tc.name)
	}
}
