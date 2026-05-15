package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

// TestMulticluster_DNSVIP_ContextSuffix verifies that context-suffixed FQDNs
// route to the correct cluster via DNS+VIP.
func TestMulticluster_DNSVIP_ContextSuffix(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "svc-alpha")
	deployNginx(t, sharedB.clientset, ns, "svc-beta")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// Test context-suffixed access to cluster A.
	body := httpGetViaDNSVIP(t, srv.DNSAddr, fmt.Sprintf("http://svc-alpha-clusterip.%s.svc.cluster.local.%s/", ns, sharedA.context))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx from cluster A, got: %s", body)
	}
	t.Logf("Cluster A via DNS+VIP context suffix: OK")

	// Test context-suffixed access to cluster B.
	body = httpGetViaDNSVIP(t, srv.DNSAddr, fmt.Sprintf("http://svc-beta-clusterip.%s.svc.cluster.local.%s/", ns, sharedB.context))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx from cluster B, got: %s", body)
	}
	t.Logf("Cluster B via DNS+VIP context suffix: OK")
}

// TestMulticluster_SOCKS5_ContextSuffix verifies that context-suffixed FQDNs
// route to the correct cluster via SOCKS5.
func TestMulticluster_SOCKS5_ContextSuffix(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "svc-alpha")
	deployNginx(t, sharedB.clientset, ns, "svc-beta")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// Test SOCKS5 with context suffix.
	body := httpGetViaSOCKS5(t, srv.SOCKSAddr, fmt.Sprintf("http://svc-alpha-clusterip.%s.svc.cluster.local.%s/", ns, sharedA.context))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx from cluster A, got: %s", body)
	}
	t.Logf("Cluster A via SOCKS5 context suffix: OK")

	body = httpGetViaSOCKS5(t, srv.SOCKSAddr, fmt.Sprintf("http://svc-beta-clusterip.%s.svc.cluster.local.%s/", ns, sharedB.context))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx from cluster B, got: %s", body)
	}
	t.Logf("Cluster B via SOCKS5 context suffix: OK")
}

// TestMulticluster_KubeconfigLifecycle verifies that the proxy correctly
// handles kubeconfig mutations: PUT, DELETE, and re-PUT with a different cluster.
func TestMulticluster_KubeconfigLifecycle(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	// Deploy nginx on cluster A.
	deployNginx(t, sharedA.clientset, ns, "nginx")

	// PUT cluster A config and verify connectivity.
	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	body := httpGetViaSOCKS5(t, srv.SOCKSAddr, fmt.Sprintf("http://nginx-clusterip.%s.svc.cluster.local/", ns))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response from cluster A, got: %s", body)
	}
	t.Logf("Cluster A connectivity: OK")

	// DELETE kubeconfig and allow tunnels to tear down.
	deleteKubeconfig(t, srv.HTTPAddr)
	time.Sleep(2 * time.Second)

	// Deploy nginx on cluster B.
	deployNginx(t, sharedB.clientset, ns, "nginx")

	// PUT cluster B config and verify connectivity.
	putKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)
	body = httpGetViaSOCKS5(t, srv.SOCKSAddr, fmt.Sprintf("http://nginx-clusterip.%s.svc.cluster.local/", ns))
	if !strings.Contains(body, "nginx") {
		t.Fatalf("expected nginx response from cluster B, got: %s", body)
	}
	t.Logf("Cluster B connectivity after lifecycle: OK")
}

// ---------------------------------------------------------------------------
// Bare hostname (all-cluster) DNS tests
// ---------------------------------------------------------------------------

// TestMulticluster_BareHostname_MultipleARecords verifies that a bare cluster
// FQDN (no context suffix) resolves to one A record per cluster that has the
// service, and that the Additional section contains a TXT metadata record for
// each VIP. It also confirms that traffic actually reaches nginx via both VIPs.
func TestMulticluster_BareHostname_MultipleARecords(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "shared")
	deployNginx(t, sharedB.clientset, ns, "shared")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("shared-clusterip.%s.svc.cluster.local", ns)

	// A query: expect one VIP per cluster.
	msg := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 2)

	ips := extractARecordIPs(msg)
	if len(ips) != 2 {
		t.Fatalf("expected 2 A records, got %d", len(ips))
	}
	if ips[0].Equal(ips[1]) {
		t.Fatalf("expected distinct VIPs, got both %s", ips[0])
	}
	t.Logf("A records: %v", ips)

	// Additional section must carry a TXT record for each VIP.
	txtExtra := extractTXTStrings(msg.Extra)
	if len(txtExtra) != 2 {
		t.Fatalf("expected 2 TXT records in Additional, got %d: %v", len(txtExtra), txtExtra)
	}
	for _, txt := range txtExtra {
		if !strings.HasPrefix(txt, "ip=") || !strings.Contains(txt, " context=") {
			t.Errorf("unexpected TXT format: %q", txt)
		}
	}
	t.Logf("Additional TXT: %v", txtExtra)

	// Both VIPs must actually serve traffic.
	for _, ip := range ips {
		body := httpGetToVIP(t, ip, 80)
		if !strings.Contains(body, "nginx") {
			t.Errorf("VIP %s: expected nginx response, got: %s", ip, body)
		}
		t.Logf("VIP %s: OK", ip)
	}
}

// TestMulticluster_BareHostname_ServiceOnlyInOneCluster verifies that when a
// service exists in only one of two registered clusters, the bare A query
// returns exactly one A record (from the cluster that has the service).
func TestMulticluster_BareHostname_ServiceOnlyInOneCluster(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	// Cluster A has "only-in-a-clusterip"; cluster B has a different service.
	deployNginx(t, sharedA.clientset, ns, "only-in-a")
	deployNginx(t, sharedB.clientset, ns, "other-svc") // different name — won't match the query

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("only-in-a-clusterip.%s.svc.cluster.local", ns)

	msg := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 1)

	if n := countARecords(msg); n != 1 {
		t.Fatalf("expected exactly 1 A record, got %d", n)
	}
	t.Logf("Single A record from the cluster that has the service: %v", extractARecordIPs(msg))
}

// TestMulticluster_BareHostname_TXT_AfterAQuery verifies that after a bare A
// query has allocated VIPs, a subsequent TXT query for the same hostname returns
// the allocated VIPs as TXT Answer records (ip=... context=... per entry).
func TestMulticluster_BareHostname_TXT_AfterAQuery(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "txtsvc")
	deployNginx(t, sharedB.clientset, ns, "txtsvc")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("txtsvc-clusterip.%s.svc.cluster.local", ns)

	// Trigger VIP allocation with an A query.
	aMsg := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 2)
	aIPs := extractARecordIPs(aMsg)

	// Now query TXT — must return the same VIPs without any new allocation.
	txtMsg := dnsLookupRaw(t, srv.DNSAddr, fqdn, mdns.TypeTXT)
	if txtMsg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR for TXT query, got rcode %d", txtMsg.Rcode)
	}
	txtStrings := extractTXTStrings(txtMsg.Answer)
	if len(txtStrings) != 2 {
		t.Fatalf("expected 2 TXT records in Answer, got %d: %v", len(txtStrings), txtStrings)
	}

	// Every A-record IP must appear in one of the TXT strings.
	for _, ip := range aIPs {
		found := false
		for _, txt := range txtStrings {
			if strings.Contains(txt, fmt.Sprintf("ip=%s ", ip.String())) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("VIP %s from A query not found in TXT records: %v", ip, txtStrings)
		}
	}
	t.Logf("TXT records match A records: %v", txtStrings)
}

// TestMulticluster_BareHostname_TXT_BeforeAQuery_NODATA verifies that when
// services exist but no VIPs have been allocated yet, a TXT query returns
// NOERROR with an empty Answer section (NODATA) — not NXDOMAIN.
func TestMulticluster_BareHostname_TXT_BeforeAQuery_NODATA(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "nodata-svc")
	deployNginx(t, sharedB.clientset, ns, "nodata-svc")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// No A query issued first; VIPs are not allocated.
	fqdn := fmt.Sprintf("nodata-svc-clusterip.%s.svc.cluster.local", ns)
	msg := dnsLookupRaw(t, srv.DNSAddr, fqdn, mdns.TypeTXT)

	if msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR (NODATA) but got rcode %d", msg.Rcode)
	}
	if len(extractTXTStrings(msg.Answer)) != 0 {
		t.Fatalf("expected empty TXT Answer (NODATA), got: %v", extractTXTStrings(msg.Answer))
	}
	t.Log("TXT before A query: NODATA as expected")
}

// TestMulticluster_BareHostname_TXT_NXDOMAIN verifies that a bare TXT query for
// a hostname whose service does not exist in any registered cluster returns
// NXDOMAIN.
func TestMulticluster_BareHostname_TXT_NXDOMAIN(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	// Register two clusters but don't create the queried service in either.
	deployNginx(t, sharedA.clientset, ns, "other-svc")
	deployNginx(t, sharedB.clientset, ns, "other-svc")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	msg := dnsLookupRaw(t, srv.DNSAddr, fmt.Sprintf("ghost-svc.%s.svc.cluster.local", ns), mdns.TypeTXT)
	if msg.Rcode != mdns.RcodeNameError {
		t.Fatalf("expected NXDOMAIN (rcode 3), got rcode %d", msg.Rcode)
	}
	t.Log("TXT for unknown service: NXDOMAIN as expected")
}

// ---------------------------------------------------------------------------
// Context-suffix TXT tests
// ---------------------------------------------------------------------------

// TestMulticluster_ContextSuffix_TXTInAdditional verifies that a context-suffixed
// A query returns the VIP in the Answer section and a TXT metadata record in the
// Additional section.
func TestMulticluster_ContextSuffix_TXTInAdditional(t *testing.T) {
	srv := startProxy(t)
	ctxName, ns := setupSingleCluster(t, srv, "ctxnginx")

	fqdn := fmt.Sprintf("ctxnginx-clusterip.%s.svc.cluster.local.%s", ns, ctxName)

	// Retry until we get the A record (gives the VIP time to be allocated).
	msg := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 1)

	if n := countARecords(msg); n != 1 {
		t.Fatalf("expected 1 A record, got %d", n)
	}

	txtExtra := extractTXTStrings(msg.Extra)
	if len(txtExtra) != 1 {
		t.Fatalf("expected 1 TXT record in Additional section, got %d: %v", len(txtExtra), txtExtra)
	}
	txt := txtExtra[0]
	if !strings.Contains(txt, fmt.Sprintf("context=%s", ctxName)) {
		t.Errorf("TXT record %q does not contain expected context %q", txt, ctxName)
	}
	if !strings.HasPrefix(txt, "ip=") {
		t.Errorf("TXT record %q does not start with ip=", txt)
	}
	t.Logf("Context-suffix A response has TXT in Additional: %s", txt)
}

// TestMulticluster_ContextSuffix_TXT_DirectQuery verifies that after a
// context-suffixed A query has allocated a VIP, a context-suffixed TXT query
// for the same name returns the VIP as a TXT Answer record.
func TestMulticluster_ContextSuffix_TXT_DirectQuery(t *testing.T) {
	srv := startProxy(t)
	ctxName, ns := setupSingleCluster(t, srv, "txtnginx")

	fqdn := fmt.Sprintf("txtnginx-clusterip.%s.svc.cluster.local.%s", ns, ctxName)

	// Allocate the VIP first via an A query.
	_ = dnsLookupAExpect(t, srv.DNSAddr, fqdn, 1)

	// Now a direct TXT query must return the allocated VIP.
	txtMsg := dnsLookupRaw(t, srv.DNSAddr, fqdn, mdns.TypeTXT)
	if txtMsg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR for context-suffix TXT, got rcode %d", txtMsg.Rcode)
	}
	txts := extractTXTStrings(txtMsg.Answer)
	if len(txts) != 1 {
		t.Fatalf("expected 1 TXT Answer record, got %d: %v", len(txts), txts)
	}
	if !strings.Contains(txts[0], fmt.Sprintf("context=%s", ctxName)) {
		t.Errorf("TXT %q missing expected context %q", txts[0], ctxName)
	}
	t.Logf("Context-suffix TXT direct query: %s", txts[0])
}

// TestMulticluster_ContextSuffix_TXT_WrongContext_NXDOMAIN verifies that a
// context-suffixed TXT query for a service that exists in cluster A but not
// cluster B returns NXDOMAIN when the B context suffix is used.
func TestMulticluster_ContextSuffix_TXT_WrongContext_NXDOMAIN(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "wrnginx")
	// Don't deploy wrnginx in cluster B.

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("wrnginx-clusterip.%s.svc.cluster.local.%s", ns, sharedB.context)

	msg := dnsLookupRaw(t, srv.DNSAddr, fqdn, mdns.TypeTXT)
	if msg.Rcode != mdns.RcodeNameError {
		t.Fatalf("expected NXDOMAIN for service absent in specified context, got rcode %d", msg.Rcode)
	}
	t.Log("TXT with wrong context suffix: NXDOMAIN as expected")
}

// TestMulticluster_BareHostname_VIPsAreDeduplicated verifies that issuing the
// same bare A query twice returns identical VIPs — the allocator must not create
// new addresses on the second query.
func TestMulticluster_BareHostname_VIPsAreDeduplicated(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "dedup")
	deployNginx(t, sharedB.clientset, ns, "dedup")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("dedup-clusterip.%s.svc.cluster.local", ns)

	// First query — allocates VIPs.
	msg1 := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 2)
	ips1 := extractARecordIPs(msg1)

	// Second query — must return the same VIPs.
	msg2 := dnsLookupAExpect(t, srv.DNSAddr, fqdn, 2)
	ips2 := extractARecordIPs(msg2)

	// Build IP sets for order-independent comparison.
	set1 := make(map[string]struct{}, len(ips1))
	for _, ip := range ips1 {
		set1[ip.String()] = struct{}{}
	}
	for _, ip := range ips2 {
		if _, ok := set1[ip.String()]; !ok {
			t.Errorf("second A query returned new VIP %s not seen in first query (first: %v)", ip, ips1)
		}
	}
	if len(ips1) != len(ips2) {
		t.Errorf("first query returned %d IPs, second returned %d", len(ips1), len(ips2))
	}
	t.Logf("VIPs are stable across queries: %v", ips1)
}

// ---------------------------------------------------------------------------
// Multicluster SRV tests
// ---------------------------------------------------------------------------

// TestMulticluster_SRV_NoContext verifies that after a bare TypeA query
// allocates VIPs for two clusters, a bare TypeSRV query returns one SRV record
// per (cluster × port), with Targets pointing at the context-suffixed FQDNs.
func TestMulticluster_SRV_NoContext(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "svc-mc")
	deployNginx(t, sharedB.clientset, ns, "svc-mc")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	fqdn := fmt.Sprintf("svc-mc-clusterip.%s.svc.cluster.local.", ns)

	// Trigger bare TypeA to allocate VIPs in both clusters.
	dnsLookupAExpect(t, srv.DNSAddr, fqdn, 2)

	// Bare TypeSRV — should return one SRV per (VIP × port) = 2 records (port
	// 80 each).
	srvRecs := dnsLookupSRVExpect(t, srv.DNSAddr, fqdn, 2)

	// Collect the set of Targets; both context-suffixed FQDNs must appear.
	targets := make(map[string]bool)
	for _, s := range srvRecs {
		targets[s.Target] = true
	}
	for _, ctx := range []string{sharedA.context, sharedB.context} {
		want := fmt.Sprintf("svc-mc-clusterip.%s.svc.cluster.local.%s.", ns, ctx)
		if !targets[want] {
			t.Errorf("expected SRV Target %q, got targets: %v", want, targets)
		}
	}
	t.Logf("Multicluster bare SRV: OK (targets: %v)", targets)
}

// TestMulticluster_SRV_ContextSpecified verifies that a context-suffixed TypeSRV
// query returns only SRV records from the specified cluster.
func TestMulticluster_SRV_ContextSpecified(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "svc-ctx")
	deployNginx(t, sharedB.clientset, ns, "svc-ctx")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// Allocate VIPs by querying the bare name first.
	bareFQDN := fmt.Sprintf("svc-ctx-clusterip.%s.svc.cluster.local.", ns)
	dnsLookupAExpect(t, srv.DNSAddr, bareFQDN, 2)

	// Context-suffixed SRV — only sharedA records expected.
	qname := fmt.Sprintf("svc-ctx-clusterip.%s.svc.cluster.local.%s.", ns, sharedA.context)
	srvRecs := dnsLookupSRVExpect(t, srv.DNSAddr, qname, 1)

	for _, s := range srvRecs {
		wantTarget := fmt.Sprintf("svc-ctx-clusterip.%s.svc.cluster.local.%s.", ns, sharedA.context)
		if s.Target != wantTarget {
			t.Errorf("SRV Target = %q, want %q", s.Target, wantTarget)
		}
	}
	if len(srvRecs) != 1 {
		t.Errorf("expected exactly 1 SRV record for context-specific query, got %d", len(srvRecs))
	}
	t.Logf("Multicluster context-specific SRV: OK")
}

// ---------------------------------------------------------------------------
// Cluster replacement isolation test
// ---------------------------------------------------------------------------

// TestMulticluster_ClusterReplacement_PreservesOtherConnections verifies the
// core isolation guarantee of per-cluster teardown: an active TCP connection
// to cluster A must survive a cluster B replacement uninterrupted, and cluster
// B must become fully operational again after re-registration.
//
// Scenario:
//  1. Start proxy with both clusters (A and B).
//  2. Open a raw TCP connection to cluster A's VIP and hold it open.
//  3. PUT only cluster A's kubeconfig — effectively "replacing" cluster B by
//     removing it from the dynamic config. This triggers ShutdownContext for B.
//  4. Assert the held cluster-A connection is still alive (not EOF'd).
//  5. Assert new cluster-A connections still work.
//  6. Re-add cluster B via PATCH.
//  7. Assert cluster B's service is reachable again with a freshly allocated VIP.
//  8. Perform a final HTTP exchange on the original held connection to prove it
//     was usable throughout.
func TestMulticluster_ClusterReplacement_PreservesOtherConnections(t *testing.T) {
	srv := startProxy(t)
	ns := testNamespace(t)

	deployNginx(t, sharedA.clientset, ns, "svc-alpha")
	deployNginx(t, sharedB.clientset, ns, "svc-beta")

	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// --- Step 1: resolve VIPs for both clusters ---

	fqdnA := fmt.Sprintf("svc-alpha-clusterip.%s.svc.cluster.local.%s", ns, sharedA.context)
	fqdnB := fmt.Sprintf("svc-beta-clusterip.%s.svc.cluster.local.%s", ns, sharedB.context)

	vipA := resolveVIP(t, srv.DNSAddr, fqdnA)
	vipB := resolveVIP(t, srv.DNSAddr, fqdnB)

	if !checkTCPConnectable(vipA, 80) {
		t.Fatalf("cluster A VIP %s:80 not reachable before replacement", vipA)
	}
	if !checkTCPConnectable(vipB, 80) {
		t.Fatalf("cluster B VIP %s:80 not reachable before replacement", vipB)
	}
	t.Logf("both clusters reachable: vipA=%s vipB=%s", vipA, vipB)

	// --- Step 2: open and hold a raw TCP connection to cluster A ---
	//
	// Accepting this connection causes the proxy to call handleConn → DialPortForward,
	// establishing a SPDY stream to an nginx pod on cluster A. The bidirectional
	// io.Copy loop starts inside the proxy goroutine, keeping the SPDY stream alive
	// as long as connA is open.

	connA, err := net.DialTimeout("tcp", fmt.Sprintf("%s:80", vipA), 10*time.Second)
	if err != nil {
		t.Fatalf("dialing cluster A VIP %s:80: %v", vipA, err)
	}
	t.Cleanup(func() { _ = connA.Close() })
	t.Logf("cluster A TCP connection open: %s -> %s", connA.LocalAddr(), connA.RemoteAddr())

	// Give the proxy a moment to finish setting up the SPDY port-forward so
	// that the connection is fully proxied before we proceed.
	time.Sleep(200 * time.Millisecond)

	// --- Step 3: replace cluster B (PUT only cluster A) ---

	t.Log("replacing cluster B: PUT kubeconfig with only cluster A")
	putKubeconfig(t, srv.HTTPAddr, sharedA.kubeconfig)

	// --- Step 4: assert connA is still alive ---
	//
	// Set a very short read deadline and read. If the proxy dropped the connection
	// we'll receive io.EOF or a closed-connection error. A deadline-exceeded error
	// (or os.ErrDeadlineExceeded) means the server hasn't sent data yet — but the
	// connection is intact.

	_ = connA.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	buf := make([]byte, 1)
	_, readErr := connA.Read(buf)
	_ = connA.SetReadDeadline(time.Time{}) // clear deadline
	if readErr != nil {
		if errors.Is(readErr, io.EOF) {
			t.Fatal("cluster A TCP connection was closed (EOF) during cluster B replacement — isolation failed")
		}
		if isNetClosedErr(readErr) {
			t.Fatalf("cluster A TCP connection was closed during cluster B replacement: %v", readErr)
		}
		// Any other error (timeout/deadline exceeded) means the connection is alive.
		t.Logf("cluster A connection alive after cluster B replacement (read returned %v as expected)", readErr)
	} else {
		t.Logf("cluster A connection alive (read 1 byte — proxy sent data early)")
	}

	// --- Step 5: new connections to cluster A still work ---

	if !checkTCPConnectable(vipA, 80) {
		t.Errorf("new cluster A connections not working after cluster B replacement")
	}
	t.Log("new cluster A connections: OK")

	// --- Step 6: re-add cluster B ---

	t.Log("re-adding cluster B via PATCH")
	patchKubeconfig(t, srv.HTTPAddr, sharedB.kubeconfig)

	// --- Step 7: cluster B reachable again with fresh VIP ---
	//
	// The old VIP-B was released when cluster B was removed; a DNS query now
	// allocates a new one (may or may not be the same address).

	newVipB := resolveVIP(t, srv.DNSAddr, fqdnB)
	t.Logf("cluster B VIP after re-add: %s", newVipB)
	waitForTCPReachable(t, newVipB, 80, 30*time.Second)
	t.Log("cluster B reachable after re-registration: OK")

	// --- Step 8: perform a full HTTP exchange on the still-held connA ---
	//
	// Send an HTTP/1.0 GET so the server closes the connection after the response,
	// giving us a clean EOF that terminates the Read loop.

	host := fmt.Sprintf("%s:80", vipA)
	_, writeErr := fmt.Fprintf(connA, "GET / HTTP/1.0\r\nHost: %s\r\n\r\n", host)
	if writeErr != nil {
		t.Fatalf("writing HTTP request on held cluster A connection: %v", writeErr)
	}
	_ = connA.SetReadDeadline(time.Now().Add(15 * time.Second))
	body, readAllErr := io.ReadAll(connA)
	if readAllErr != nil && !errors.Is(readAllErr, io.EOF) {
		t.Fatalf("reading HTTP response on held cluster A connection: %v", readAllErr)
	}
	if !strings.Contains(string(body), "nginx") && !strings.Contains(string(body), "200") {
		t.Errorf("unexpected response on held cluster A connection (want nginx/200 OK):\n%s", string(body))
	}
	t.Logf("full HTTP exchange on original connA succeeded — connection was intact throughout: OK")
}

// isNetClosedErr reports whether err looks like a "use of closed network
// connection" error, which indicates the proxy dropped the connection.
func isNetClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed")
}
