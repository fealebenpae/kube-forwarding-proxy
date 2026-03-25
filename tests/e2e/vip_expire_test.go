package e2e

import (
	"fmt"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/fealebenpae/kube-forwarding-proxy/app"
)

// vipIdleTimeout is the idle timeout used in all VIP expiry tests. It must be
// long enough to avoid false positives on slow CI but short enough not to make
// the tests unreasonably slow.
const vipIdleTimeout = 5 * time.Second

// startProxyWithIdleTimeout starts an in-process proxy with VIPIdleTimeout set.
func startProxyWithIdleTimeout(t *testing.T, d time.Duration) *app.Server {
	t.Helper()
	return startProxyCustom(t, func(c *app.Config) {
		c.VIPIdleTimeout = d
	})
}

// resolveVIP sends a DNS A query (with retries) and returns the first VIP found.
// Unlike httpGetViaDNSVIP, this makes no TCP connection to the VIP, so it does
// NOT increment activeConns and does NOT reset the idle timer via handleConn.
// After resolveVIP returns the VIP is fully allocated and its port-forward
// listener is bound (StartForward is synchronous).
func resolveVIP(t *testing.T, dnsAddr, fqdn string) net.IP {
	t.Helper()
	msg := dnsLookupAExpect(t, dnsAddr, fqdn, 1)
	ips := extractARecordIPs(msg)
	if len(ips) == 0 {
		t.Fatalf("no A records returned for %s", fqdn)
	}
	return ips[0]
}

// TestVIPExpiry_ExpiresAfterIdleTimeout verifies that a VIP and its TCP
// listener are torn down after the idle timeout elapses with no active
// connections or DNS queries.
//
// Design note: the test deliberately avoids making any TCP connections to the
// VIP during the idle wait. Any TCP connection accepted by the proxy increments
// activeConns, stops the timer, and re-arms it when the connection closes —
// polling with checkTCPConnectable would therefore prevent expiry. Instead we
// sleep for 2× the idle timeout and then do a single connectivity check.
func TestVIPExpiry_ExpiresAfterIdleTimeout(t *testing.T) {
	srv := startProxyWithIdleTimeout(t, vipIdleTimeout)
	setupSingleCluster(t, srv, "e2e-vip-expire", "nginx")

	const fqdn = "nginx-clusterip.default.svc.cluster.local"

	// Allocate VIP via DNS only (no TCP connection to the VIP).
	// dnsLookupAExpect retries until the endpoint slice is propagated, so this
	// also serves as our "wait for nginx" gate.
	vipAddr := resolveVIP(t, srv.DNSAddr, fqdn)
	t.Logf("VIP allocated: %s; idle timer armed (timeout=%s)", vipAddr, vipIdleTimeout)

	// Sleep for 2× idle timeout with no TCP connections → timer fires.
	sleepDur := vipIdleTimeout * 2
	t.Logf("sleeping %s for idle timer to fire (no TCP connections)...", sleepDur)
	time.Sleep(sleepDur)

	if checkTCPConnectable(vipAddr, 80) {
		t.Fatalf("VIP listener on %s:80 should have expired after %s idle", vipAddr, vipIdleTimeout)
	}
	t.Logf("VIP listener expired after idle timeout: OK")

	// Re-querying DNS re-allocates a new VIP and starts its listener.
	newVIP := resolveVIP(t, srv.DNSAddr, fqdn)
	t.Logf("VIP re-allocated: %s", newVIP)
	// StartForward is synchronous, so the new listener is up before resolveVIP returns.
	if !checkTCPConnectable(newVIP, 80) {
		t.Fatalf("re-allocated VIP listener on %s:80 should be reachable immediately", newVIP)
	}
	t.Logf("re-allocated VIP is reachable: OK")
}

// TestVIPExpiry_ResetByDNSQuery verifies that a DNS re-query resets the idle
// timer so the VIP survives past its original expiry deadline.
func TestVIPExpiry_ResetByDNSQuery(t *testing.T) {
	srv := startProxyWithIdleTimeout(t, vipIdleTimeout)
	setupSingleCluster(t, srv, "e2e-vip-reset", "nginx")

	const fqdn = "nginx-clusterip.default.svc.cluster.local"

	// Allocate VIP at t=0 via DNS only. Timer armed; expires at t≈5s.
	vipAddr := resolveVIP(t, srv.DNSAddr, fqdn)
	t.Logf("VIP allocated at t=0: %s; timer expires at t≈%s", vipAddr, vipIdleTimeout)

	// At t≈half, reset the idle timer via a raw DNS query. The DNS server's
	// resolveForContext hits the early-return path (VIP already allocated) and
	// calls TouchVIP, re-arming the timer at t≈half+vipIdleTimeout.
	half := vipIdleTimeout / 2 // 2.5s
	t.Logf("sleeping %s then sending DNS touch...", half)
	time.Sleep(half)
	dnsLookupRaw(t, srv.DNSAddr, fqdn, mdns.TypeA) // calls TouchVIP; new timer: t≈2.5+5=7.5s
	t.Logf("DNS touch sent at t≈%s; timer reset to t≈%s", half, half+vipIdleTimeout)

	// Sleep past original deadline (t=5s); elapsed ≈ half+(half+500ms)=5.5s > 5s.
	extra := half + 500*time.Millisecond
	t.Logf("sleeping another %s (past original deadline, before reset deadline)...", extra)
	time.Sleep(extra)

	// The VIP must still be alive. Note: this checkTCPConnectable briefly stops
	// and re-arms the timer, but the key assertion holds: the original deadline
	// (t=5s) has passed yet the VIP is alive, proving the DNS touch worked.
	if !checkTCPConnectable(vipAddr, 80) {
		t.Fatalf("VIP must be alive at t≈%s; DNS touch at t≈%s extended deadline to t≈%s",
			half+extra, half, half+vipIdleTimeout)
	}
	t.Logf("VIP still alive past original deadline (timer was reset by DNS): OK")
	// checkTCPConnectable re-armed the timer at approximately t≈5.5s+5s=10.5s.

	// Sleep 2× idle timeout; re-armed timer fires well within this window.
	t.Logf("sleeping %s for re-armed timer to fire...", vipIdleTimeout*2)
	time.Sleep(vipIdleTimeout * 2) // 10s; timer fires ≈5s in

	if checkTCPConnectable(vipAddr, 80) {
		t.Fatal("VIP should have expired after the re-armed idle timer fired")
	}
	t.Logf("VIP expired after re-armed idle timer: OK")
}

// TestVIPExpiry_PausedByActiveConnection verifies that the idle timer is paused
// while at least one TCP connection is active and restarts after the last
// connection closes.
func TestVIPExpiry_PausedByActiveConnection(t *testing.T) {
	srv := startProxyWithIdleTimeout(t, vipIdleTimeout)
	setupSingleCluster(t, srv, "e2e-vip-pause", "nginx")

	const fqdn = "nginx-clusterip.default.svc.cluster.local"

	// Allocate VIP via DNS only (activeConns=0; timer armed).
	vipAddr := resolveVIP(t, srv.DNSAddr, fqdn)
	t.Logf("VIP allocated: %s; idle timer armed", vipAddr)

	// Open a raw TCP connection and hold it. handleConn increments activeConns
	// and stops the idle timer. The SPDY proxy stream to nginx stays open.
	addr := fmt.Sprintf("%s:80", vipAddr)
	holdConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatalf("dialing holding TCP connection to %s: %v", addr, err)
	}
	t.Logf("holding TCP connection open to %s; idle timer stopped", addr)

	// Sleep past the idle timeout — timer is stopped, VIP must stay alive.
	hold := vipIdleTimeout + vipIdleTimeout/2 // 7.5s
	t.Logf("sleeping %s (past idle timeout of %s) with connection held...", hold, vipIdleTimeout)
	time.Sleep(hold)

	// Close the holding connection. handleConn's deferred cleanup decrements
	// activeConns to 0 and re-arms the idle timer.
	t.Logf("closing holding connection; re-arming idle timer...")
	holdConn.Close()

	// Sleep 2× idle timeout with no further TCP connections; re-armed timer fires.
	sleepDur := vipIdleTimeout * 2 // 10s; timer fires ~5s in
	t.Logf("sleeping %s for idle timer to fire after connection close...", sleepDur)
	time.Sleep(sleepDur)

	if checkTCPConnectable(vipAddr, 80) {
		t.Fatal("VIP listener should have expired after holding connection was closed and idle timer fired")
	}
	t.Logf("VIP expired after holding connection was closed: OK")
}
