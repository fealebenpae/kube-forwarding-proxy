package e2e

import (
	"fmt"
	"strings"
	"testing"

	mdns "github.com/miekg/dns"
)

// TestDNSVIP_ClusterIP verifies that the proxy's DNS server returns a VIP for a
// ClusterIP service and that the VIP listener tunnels traffic to the pod.
func TestDNSVIP_ClusterIP(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-dns-cip", "nginx")

	body := httpGetViaDNSVIP(t, srv.DNSAddr, "http://nginx-clusterip.default.svc.cluster.local/")
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response, got: %s", body)
	}
	t.Logf("ClusterIP via DNS+VIP: OK")
}

// TestDNSVIP_HeadlessPod verifies that a pod-specific headless FQDN resolves
// to a VIP and traffic reaches the correct pod.
func TestDNSVIP_HeadlessPod(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-dns-hl", "nginx")

	body := httpGetViaDNSVIP(t, srv.DNSAddr, "http://nginx-0.nginx-headless.default.svc.cluster.local/")
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response, got: %s", body)
	}
	t.Logf("Headless pod via DNS+VIP: OK")
}

// TestDNSVIP_NonClusterForwarding verifies that non-cluster DNS queries are
// forwarded to upstream nameservers.
func TestDNSVIP_NonClusterForwarding(t *testing.T) {
	srv := startProxy(t)

	body := httpGetViaDNSVIP(t, srv.DNSAddr, "http://example.com/")
	if body == "" {
		t.Fatal("expected non-empty response from example.com")
	}
	t.Logf("Non-cluster DNS forwarding: OK (got %d bytes)", len(body))
}

// TestDNSVIP_MultiPort deploys two nginx containers in a single pod — one
// serving on port 80 and one on port 8080 — behind a single ClusterIP service
// that exposes both ports. It verifies that both ports are reachable via DNS+VIP.
func TestDNSVIP_MultiPort(t *testing.T) {
	srv := startProxy(t)
	setupSingleClusterMultiPort(t, srv, "e2e-dns-mp", "nginx-mp")

	for _, tc := range []struct {
		port int
		name string
	}{
		{80, "port 80"},
		{8080, "port 8080"},
	} {
		url := fmt.Sprintf("http://nginx-mp-clusterip.default.svc.cluster.local:%d/", tc.port)
		body := httpGetViaDNSVIP(t, srv.DNSAddr, url)
		if !strings.Contains(body, "nginx") {
			t.Fatalf("expected nginx response on %s, got: %s", tc.name, body)
		}
		t.Logf("MultiPort %s via DNS+VIP: OK", tc.name)
	}
}

// ---------------------------------------------------------------------------
// SRV record tests
// ---------------------------------------------------------------------------

// TestDNSVIP_SRV_AfterAQuery verifies that after a TypeA query allocates a VIP
// a subsequent TypeSRV query for the same name returns SRV records whose Port
// field matches the service's port and whose Target is the context-suffixed FQDN.
func TestDNSVIP_SRV_AfterAQuery(t *testing.T) {
	srv := startProxy(t)
	ctxName := setupSingleCluster(t, srv, "e2e-srv-after-a", "nginx")

	const qname = "nginx-clusterip.default.svc.cluster.local."

	// Trigger A query to allocate the VIP.
	httpGetViaDNSVIP(t, srv.DNSAddr, "http://nginx-clusterip.default.svc.cluster.local/")

	// Now query SRV and expect at least one record for port 80.
	srvRecs := dnsLookupSRVExpect(t, srv.DNSAddr, qname, 1)

	portFound := false
	for _, s := range srvRecs {
		if s.Port == 80 {
			portFound = true
		}
		wantTarget := "nginx-clusterip.default.svc.cluster.local." + ctxName + "."
		if s.Target != wantTarget {
			t.Errorf("SRV Target = %q, want %q", s.Target, wantTarget)
		}
	}
	if !portFound {
		t.Errorf("expected SRV record with Port=80, got: %v", srvRecs)
	}
	t.Logf("SRV after A query: OK (%d record(s))", len(srvRecs))
}

// TestDNSVIP_SRV_DirectQuery_NXDOMAIN verifies that a TypeSRV query for a
// non-existent service returns an NXDOMAIN response.
func TestDNSVIP_SRV_DirectQuery_NXDOMAIN(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-srv-nxd", "nginx")

	const qname = "does-not-exist.default.svc.cluster.local."
	msg := dnsLookupRaw(t, srv.DNSAddr, qname, mdns.TypeSRV)
	if msg == nil {
		t.Fatal("no response received")
	}
	if msg.Rcode != mdns.RcodeNameError {
		t.Errorf("expected NXDOMAIN (Rcode 3) for missing service, got Rcode %d", msg.Rcode)
	}
	t.Logf("SRV NXDOMAIN: OK")
}

// TestDNSVIP_SRV_DirectQuery_NODATA verifies that a TypeSRV query for a service
// that exists but has no allocated VIP yet returns NOERROR with an empty Answer
// (NODATA semantics).
func TestDNSVIP_SRV_DirectQuery_NODATA(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-srv-nodata", "nginx")

	// Do NOT issue any TypeA query — no VIP should be allocated yet.
	const qname = "nginx-clusterip.default.svc.cluster.local."
	msg := dnsLookupRaw(t, srv.DNSAddr, qname, mdns.TypeSRV)
	if msg == nil {
		t.Fatal("no response received")
	}
	if msg.Rcode != mdns.RcodeSuccess {
		t.Errorf("expected NOERROR (NODATA) for existing service without VIP, got Rcode %d", msg.Rcode)
	}
	for _, rr := range msg.Answer {
		if _, ok := rr.(*mdns.SRV); ok {
			t.Errorf("expected no SRV answers (NODATA), got: %v", rr)
		}
	}
	t.Logf("SRV NODATA: OK")
}

// TestDNSVIP_SRV_TypeA_ExtraSection verifies that the Additional/Extra section
// of a TypeA response contains SRV records alongside the TXT records.
func TestDNSVIP_SRV_TypeA_ExtraSection(t *testing.T) {
	srv := startProxy(t)
	setupSingleCluster(t, srv, "e2e-srv-extra", "nginx")

	const qname = "nginx-clusterip.default.svc.cluster.local."

	// TypeA query — triggers VIP allocation.
	msg := dnsLookupRaw(t, srv.DNSAddr, qname, mdns.TypeA)
	if msg == nil {
		t.Fatal("no response for TypeA")
	}
	if msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("TypeA Rcode = %d, want NOERROR", msg.Rcode)
	}

	srvExtras := extractSRVFromExtra(msg)
	if len(srvExtras) == 0 {
		t.Error("expected SRV records in Extra section of TypeA response, got none")
	}
	for _, s := range srvExtras {
		if s.Port == 0 {
			t.Errorf("SRV record in Extra has Port=0")
		}
	}
	t.Logf("SRV in TypeA Extra: OK (%d record(s))", len(srvExtras))
}

// TestDNSVIP_SRV_MultiPort verifies that a service exposing two ports generates
// two SRV records (one per port) after a TypeA query allocates the VIP.
func TestDNSVIP_SRV_MultiPort(t *testing.T) {
	srv := startProxy(t)
	setupSingleClusterMultiPort(t, srv, "e2e-srv-mp", "nginx-mp")

	// Trigger TypeA to allocate the VIP.
	httpGetViaDNSVIP(t, srv.DNSAddr, "http://nginx-mp-clusterip.default.svc.cluster.local/")

	const qname = "nginx-mp-clusterip.default.svc.cluster.local."
	srvRecs := dnsLookupSRVExpect(t, srv.DNSAddr, qname, 2)

	ports := make(map[uint16]bool)
	for _, s := range srvRecs {
		ports[s.Port] = true
	}
	for _, want := range []uint16{80, 8080} {
		if !ports[want] {
			t.Errorf("expected SRV port %d, not found in records: %v", want, srvRecs)
		}
	}
	t.Logf("SRV MultiPort: OK (ports %v)", ports)
}
