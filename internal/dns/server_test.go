package dns

import (
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/proxy"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/vip"
)

func TestParseServiceFQDN_Service(t *testing.T) {
	s := &Server{clusterDomain: "svc.cluster.local"}

	tests := []struct {
		input   string
		wantPod string
		wantSvc string
		wantNS  string
		wantOK  bool
	}{
		{"my-svc.default.svc.cluster.local", "", "my-svc", "default", true},
		{"redis.kube-system.svc.cluster.local", "", "redis", "kube-system", true},
		{"api-server.production.svc.cluster.local", "", "api-server", "production", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			pod, svc, ns, _, ok := proxy.ParseClusterHost(tt.input, s.clusterDomain)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if pod != tt.wantPod {
				t.Errorf("pod = %q, want %q", pod, tt.wantPod)
			}
			if svc != tt.wantSvc {
				t.Errorf("svc = %q, want %q", svc, tt.wantSvc)
			}
			if ns != tt.wantNS {
				t.Errorf("ns = %q, want %q", ns, tt.wantNS)
			}
		})
	}
}

func TestParseServiceFQDN_HeadlessPod(t *testing.T) {
	s := &Server{clusterDomain: "svc.cluster.local"}

	tests := []struct {
		input   string
		wantPod string
		wantSvc string
		wantNS  string
		wantOK  bool
	}{
		{"pod-0.my-svc.default.svc.cluster.local", "pod-0", "my-svc", "default", true},
		{"redis-0.redis.kube-system.svc.cluster.local", "redis-0", "redis", "kube-system", true},
		{"web-2.api-server.production.svc.cluster.local", "web-2", "api-server", "production", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			pod, svc, ns, _, ok := proxy.ParseClusterHost(tt.input, s.clusterDomain)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if pod != tt.wantPod {
				t.Errorf("pod = %q, want %q", pod, tt.wantPod)
			}
			if svc != tt.wantSvc {
				t.Errorf("svc = %q, want %q", svc, tt.wantSvc)
			}
			if ns != tt.wantNS {
				t.Errorf("ns = %q, want %q", ns, tt.wantNS)
			}
		})
	}
}

func TestParseServiceFQDN_Invalid(t *testing.T) {
	s := &Server{clusterDomain: "svc.cluster.local"}

	tests := []struct {
		name  string
		input string
	}{
		{"no cluster domain", "my-svc.default.example.com"},
		{"only service name", "my-svc.svc.cluster.local"},
		{"empty parts", ".svc.cluster.local"},
		{"completely wrong", "google.com"},
		{"just cluster domain", "svc.cluster.local"},
		{"four labels before domain", "extra.pod.svc.ns.svc.cluster.local"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, _, ok := proxy.ParseClusterHost(tt.input, s.clusterDomain)
			if ok {
				t.Errorf("expected ok=false for %q", tt.input)
			}
		})
	}
}

// fakeResponseWriter captures the DNS message written by a handler.
type fakeResponseWriter struct {
	msg *mdns.Msg
}

func (f *fakeResponseWriter) LocalAddr() net.Addr         { return &net.UDPAddr{} }
func (f *fakeResponseWriter) RemoteAddr() net.Addr        { return &net.UDPAddr{} }
func (f *fakeResponseWriter) WriteMsg(m *mdns.Msg) error  { f.msg = m; return nil }
func (f *fakeResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeResponseWriter) Close() error                { return nil }
func (f *fakeResponseWriter) TsigStatus() error           { return nil }
func (f *fakeResponseWriter) TsigTimersOnly(bool)         {}
func (f *fakeResponseWriter) Hijack()                     {}

// TestHandleCluster_AAAAReturnsNODATA verifies that a AAAA query for a name
// under the cluster domain gets a NOERROR/empty-answer response (NODATA), not
// NXDOMAIN. Returning NXDOMAIN for AAAA causes clients that send A and AAAA
// queries in parallel to treat the name as non-existent and skip the A lookup.
func TestHandleCluster_AAAAReturnsNODATA(t *testing.T) {
	srv := &Server{
		clusterDomain: "svc.cluster.local",
		// allocator and resolver are nil; handleCluster only calls resolveA for TypeA questions.
	}

	req := new(mdns.Msg)
	req.SetQuestion("nginx.default.svc.cluster.local.", mdns.TypeAAAA)

	w := &fakeResponseWriter{}
	srv.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Errorf("expected NOERROR (Rcode 0) for AAAA query, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 0 {
		t.Errorf("expected empty answer section for AAAA query, got %d records", len(w.msg.Answer))
	}
}

// TestHandleCluster_TypeANXDOMAIN verifies that when resolveA returns nil (name
// not found) for a TypeA query the response carries NXDOMAIN.
// The name has only one label before the cluster domain so ParseClusterHost
// returns ok=false and resolveA returns nil without touching the allocator.
func TestHandleCluster_TypeANXDOMAIN(t *testing.T) {
	srv := &Server{
		clusterDomain: "svc.cluster.local",
		// allocator and resolver are nil; resolveA returns nil before reaching
		// them because ParseClusterHost returns ok=false for a name with a
		// single label before the cluster domain suffix.
	}

	// "onelabel.svc.cluster.local" has prefix "onelabel" (1 part) which fails
	// the 2-or-3 labels requirement in ParseClusterHost → resolveA returns nil.
	req := new(mdns.Msg)
	req.SetQuestion("onelabel.svc.cluster.local.", mdns.TypeA)

	w := &fakeResponseWriter{}
	srv.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeNameError {
		t.Errorf("expected NXDOMAIN (Rcode 3) for unresolvable TypeA query, got Rcode %d", w.msg.Rcode)
	}
}

// ---------------------------------------------------------------------------
// Helpers shared by SRV tests
// ---------------------------------------------------------------------------

// noopAddrManager is a vip.InterfaceAddressManager that never fails; used so
// vip.Allocator can be constructed without a real network interface.
type noopAddrManager struct{}

func (noopAddrManager) AddAddress(net.IP) error    { return nil }
func (noopAddrManager) RemoveAddress(net.IP) error { return nil }

// newTestAllocator builds a vip.Allocator backed by the no-op address manager.
func newTestAllocator(t *testing.T) *vip.Allocator {
	t.Helper()
	a, err := vip.NewAllocator(noopAddrManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	return a
}

// oneEndpoint returns a minimal ResolvedService with a single fake endpoint and
// the given ports, suitable for seeding a test Resolver.
func oneEndpoint(ports ...int32) *k8s.ResolvedService {
	svcPorts := make([]k8s.ServicePort, len(ports))
	for i, p := range ports {
		svcPorts[i] = k8s.ServicePort{Port: p, TargetPort: p, Protocol: "TCP"}
	}
	return &k8s.ResolvedService{
		Endpoints: []k8s.Endpoint{{PodName: "pod-0", Namespace: "default", IP: "10.0.0.1"}},
		Ports:     svcPorts,
		CachedAt:  time.Now(),
	}
}

// ---------------------------------------------------------------------------
// TypeA Extra contains SRV records
// ---------------------------------------------------------------------------

// TestHandleCluster_TypeA_SRVInExtra verifies that a TypeA response includes
// one SRV record per service port in the Extra section, alongside the TXT record.
func TestHandleCluster_TypeA_SRVInExtra(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		contextName   = "ctx-a"
		qname         = "svc.default.svc.cluster.local."
	)

	alloc := newTestAllocator(t)
	key := contextName + "/default/svc"
	vipAddr, err := alloc.Allocate(key)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	resolver := k8s.NewResolverForTest([]string{contextName})
	resolver.Seed(contextName, "default", "svc", oneEndpoint(80, 8080))

	srv := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	req := new(mdns.Msg)
	req.SetQuestion(qname, mdns.TypeA)
	w := &fakeResponseWriter{}
	srv.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got Rcode %d", w.msg.Rcode)
	}

	// Exactly one A answer.
	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 A answer, got %d", len(w.msg.Answer))
	}
	aRec, ok := w.msg.Answer[0].(*mdns.A)
	if !ok {
		t.Fatalf("answer[0] is %T, want *mdns.A", w.msg.Answer[0])
	}
	if !aRec.A.Equal(vipAddr) {
		t.Errorf("A record IP = %s, want %s", aRec.A, vipAddr)
	}

	// Extra should have: 1 TXT + 2 SRV (one per port).
	var txtCount, srvCount int
	for _, rr := range w.msg.Extra {
		switch rr.Header().Rrtype {
		case mdns.TypeTXT:
			txtCount++
		case mdns.TypeSRV:
			srvCount++
			srv := rr.(*mdns.SRV)
			wantTarget := "svc.default." + clusterDomain + "." + contextName + "."
			if srv.Target != wantTarget {
				t.Errorf("SRV Target = %q, want %q", srv.Target, wantTarget)
			}
		}
	}
	if txtCount != 1 {
		t.Errorf("Extra TXT count = %d, want 1", txtCount)
	}
	if srvCount != 2 {
		t.Errorf("Extra SRV count = %d, want 2 (one per port)", srvCount)
	}

	// Verify both ports appear.
	ports := make(map[uint16]bool)
	for _, rr := range w.msg.Extra {
		if s, ok := rr.(*mdns.SRV); ok {
			ports[s.Port] = true
		}
	}
	for _, want := range []uint16{80, 8080} {
		if !ports[want] {
			t.Errorf("expected SRV port %d in Extra, not found", want)
		}
	}
}

// TestHandleCluster_TypeA_SRVInExtra_MultiContext verifies that when two
// contexts each have a VIP for the same bare name, both sets of SRV records
// appear in the Extra section.
func TestHandleCluster_TypeA_SRVInExtra_MultiContext(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		ctxA          = "ctx-a"
		ctxB          = "ctx-b"
		qname         = "svc.default.svc.cluster.local."
	)

	alloc := newTestAllocator(t)
	if _, err := alloc.Allocate(ctxA + "/default/svc"); err != nil {
		t.Fatalf("Allocate ctx-a: %v", err)
	}
	if _, err := alloc.Allocate(ctxB + "/default/svc"); err != nil {
		t.Fatalf("Allocate ctx-b: %v", err)
	}

	resolver := k8s.NewResolverForTest([]string{ctxA, ctxB})
	resolver.Seed(ctxA, "default", "svc", oneEndpoint(80))
	resolver.Seed(ctxB, "default", "svc", oneEndpoint(80))

	srv := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	req := new(mdns.Msg)
	req.SetQuestion(qname, mdns.TypeA)
	w := &fakeResponseWriter{}
	srv.handleCluster(w, req)

	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 2 {
		t.Fatalf("expected 2 A answers (one per context), got %d", len(w.msg.Answer))
	}

	// 2 TXT + 2 SRV (one SRV per VIP×port, 1 port each).
	var txtCount, srvCount int
	for _, rr := range w.msg.Extra {
		switch rr.Header().Rrtype {
		case mdns.TypeTXT:
			txtCount++
		case mdns.TypeSRV:
			srvCount++
		}
	}
	if txtCount != 2 {
		t.Errorf("Extra TXT count = %d, want 2", txtCount)
	}
	if srvCount != 2 {
		t.Errorf("Extra SRV count = %d, want 2 (one per VIP)", srvCount)
	}
}

// ---------------------------------------------------------------------------
// Direct TypeSRV queries
// ---------------------------------------------------------------------------

// TestHandleCluster_TypeSRV_AllocatedVIPs verifies that a TypeSRV query for a
// name with pre-allocated VIPs returns SRV Answer records (NOERROR).
func TestHandleCluster_TypeSRV_AllocatedVIPs(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		contextName   = "ctx-a"
		qname         = "svc.default.svc.cluster.local."
	)

	alloc := newTestAllocator(t)
	if _, err := alloc.Allocate(contextName + "/default/svc"); err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	resolver := k8s.NewResolverForTest([]string{contextName})
	resolver.Seed(contextName, "default", "svc", oneEndpoint(80, 443))

	s := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	req := new(mdns.Msg)
	req.SetQuestion(qname, mdns.TypeSRV)
	w := &fakeResponseWriter{}
	s.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 2 {
		t.Fatalf("expected 2 SRV answers (one per port), got %d", len(w.msg.Answer))
	}

	wantTarget := "svc.default." + clusterDomain + "." + contextName + "."
	for _, rr := range w.msg.Answer {
		srv, ok := rr.(*mdns.SRV)
		if !ok {
			t.Fatalf("answer is %T, want *mdns.SRV", rr)
		}
		if srv.Target != wantTarget {
			t.Errorf("SRV Target = %q, want %q", srv.Target, wantTarget)
		}
	}

	ports := make(map[uint16]bool)
	for _, rr := range w.msg.Answer {
		ports[rr.(*mdns.SRV).Port] = true
	}
	for _, want := range []uint16{80, 443} {
		if !ports[want] {
			t.Errorf("SRV port %d missing from Answer", want)
		}
	}
}

// TestHandleCluster_TypeSRV_ContextSuffixed verifies that a context-suffixed
// TypeSRV query only returns records from the specified context.
func TestHandleCluster_TypeSRV_ContextSuffixed(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		ctxA          = "ctx-a"
		ctxB          = "ctx-b"
	)

	alloc := newTestAllocator(t)
	if _, err := alloc.Allocate(ctxA + "/default/svc"); err != nil {
		t.Fatalf("Allocate ctxA: %v", err)
	}
	if _, err := alloc.Allocate(ctxB + "/default/svc"); err != nil {
		t.Fatalf("Allocate ctxB: %v", err)
	}

	resolver := k8s.NewResolverForTest([]string{ctxA, ctxB})
	resolver.Seed(ctxA, "default", "svc", oneEndpoint(80))
	resolver.Seed(ctxB, "default", "svc", oneEndpoint(9000))

	s := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	// Query with context suffix pointing only at ctxA.
	req := new(mdns.Msg)
	req.SetQuestion("svc.default.svc.cluster.local."+ctxA+".", mdns.TypeSRV)
	w := &fakeResponseWriter{}
	s.handleCluster(w, req)

	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Fatalf("expected NOERROR, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 1 {
		t.Fatalf("expected 1 SRV answer (ctxA port only), got %d", len(w.msg.Answer))
	}
	if w.msg.Answer[0].(*mdns.SRV).Port != 80 {
		t.Errorf("expected port 80 (ctxA), got %d", w.msg.Answer[0].(*mdns.SRV).Port)
	}
}

// TestHandleCluster_TypeSRV_NXDOMAIN verifies that a TypeSRV query for a name
// that cannot be resolved in any context returns NXDOMAIN.
func TestHandleCluster_TypeSRV_NXDOMAIN(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		contextName   = "ctx-a"
	)

	alloc := newTestAllocator(t)
	// Resolver has no seeded entry → Resolve will return an error.
	resolver := k8s.NewResolverForTest([]string{contextName})

	s := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	req := new(mdns.Msg)
	req.SetQuestion("missing.default.svc.cluster.local.", mdns.TypeSRV)
	w := &fakeResponseWriter{}
	s.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeNameError {
		t.Errorf("expected NXDOMAIN, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 0 {
		t.Errorf("expected empty Answer for NXDOMAIN, got %d records", len(w.msg.Answer))
	}
}

// TestHandleCluster_TypeSRV_NODATA verifies that a TypeSRV query for a service
// that exists (resolvable) but has no allocated VIPs returns NOERROR with an
// empty Answer section (NODATA).
func TestHandleCluster_TypeSRV_NODATA(t *testing.T) {
	const (
		clusterDomain = "svc.cluster.local"
		contextName   = "ctx-a"
	)

	alloc := newTestAllocator(t)
	// Service exists in resolver cache but no VIP has been allocated.
	resolver := k8s.NewResolverForTest([]string{contextName})
	resolver.Seed(contextName, "default", "svc", oneEndpoint(80))

	s := &Server{
		clusterDomain: clusterDomain,
		allocator:     alloc,
		resolver:      resolver,
	}

	req := new(mdns.Msg)
	req.SetQuestion("svc.default.svc.cluster.local.", mdns.TypeSRV)
	w := &fakeResponseWriter{}
	s.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Errorf("expected NOERROR (NODATA), got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 0 {
		t.Errorf("expected empty Answer for NODATA, got %d records", len(w.msg.Answer))
	}
}

// TestHandleCluster_AAAAStillNODATAAfterSRV is a regression guard: AAAA queries
// must still return NOERROR/empty (NODATA) after the TypeSRV case was added.
func TestHandleCluster_AAAAStillNODATAAfterSRV(t *testing.T) {
	s := &Server{clusterDomain: "svc.cluster.local"}

	req := new(mdns.Msg)
	req.SetQuestion("svc.default.svc.cluster.local.", mdns.TypeAAAA)
	w := &fakeResponseWriter{}
	s.handleCluster(w, req)

	if w.msg == nil {
		t.Fatal("no response written")
	}
	if w.msg.Rcode != mdns.RcodeSuccess {
		t.Errorf("expected NOERROR for AAAA, got Rcode %d", w.msg.Rcode)
	}
	if len(w.msg.Answer) != 0 {
		t.Errorf("expected empty Answer for AAAA NODATA, got %d records", len(w.msg.Answer))
	}
}
