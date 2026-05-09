package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/health"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/install"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
)

// newStatusTestServer builds a Server populated enough that handleStatus can
// run end-to-end without binding listeners. Returns an httptest server
// proxying /status to s.handleStatus.
func newStatusTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := Config{
		Interface:     "lo0",
		VIPCIDR:       "127.50.0.0/24",
		ClusterDomain: "svc.cluster.local",
		LogLevel:      "info",
		HTTPListen:    "127.0.0.1:11616",
		DNSListen:     "127.0.0.1:11617",
		SOCKSListen:   "127.0.0.1:11618",
		VIPAliasMode:  "preallocated",
	}
	cm, err := k8s.NewClientManager(zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	hh := &health.Handler{}
	hh.SetReady()
	s := &Server{
		cfg:           cfg,
		logger:        zap.NewNop().Sugar(),
		enableDNS:     true,
		enableSocks:   false,
		startTime:     time.Now().Add(-2 * time.Minute),
		healthHandler: hh,
		HTTPAddr:      "127.0.0.1:11616",
		DNSAddr:       "127.0.0.1:11617",
		k8sManager:    cm,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	return s, httptest.NewServer(mux)
}

func TestHandleStatus_DefaultJSON(t *testing.T) {
	_, ts := newStatusTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	var s install.Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if s.DaemonHTTP != "127.0.0.1:11616" {
		t.Errorf("DaemonHTTP = %q", s.DaemonHTTP)
	}
	if !s.DaemonReachable {
		t.Errorf("DaemonReachable = false; daemon serving its own /status should be reachable")
	}
	if !s.DaemonReady {
		t.Errorf("DaemonReady = false; SetReady was called in the test fixture")
	}
	if s.DaemonSettings == nil {
		t.Fatalf("DaemonSettings = nil")
	}
	if s.DaemonSettings.Interface != "lo0" {
		t.Errorf("DaemonSettings.Interface = %q, want lo0", s.DaemonSettings.Interface)
	}
	if s.DaemonSettings.VIPCIDR != "127.50.0.0/24" {
		t.Errorf("DaemonSettings.VIPCIDR = %q", s.DaemonSettings.VIPCIDR)
	}
	if !s.DaemonSettings.DNSEnabled {
		t.Errorf("DaemonSettings.DNSEnabled = false")
	}
	if s.DaemonPID == 0 {
		t.Errorf("DaemonPID = 0; expected os.Getpid()")
	}
	if s.DaemonUptime == "" {
		t.Errorf("DaemonUptime empty; expected non-empty for a started daemon")
	}
	// PoolFirst/Last should be filled from PoolIPs computation regardless
	// of whether the test host actually has lo0 aliased.
	if s.PoolFirst != "127.50.0.1" || s.PoolLast != "127.50.0.255" {
		t.Errorf("Pool range = %s..%s, want 127.50.0.1..127.50.0.255", s.PoolFirst, s.PoolLast)
	}
	if s.PoolTotal != 255 {
		t.Errorf("PoolTotal = %d, want 255", s.PoolTotal)
	}
	if len(s.Contexts) != 0 {
		t.Errorf("Contexts non-empty for a fresh ClientManager: %v", s.Contexts)
	}
}

func TestHandleStatus_TextFormat(t *testing.T) {
	_, ts := newStatusTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status?fmt=text")
	if err != nil {
		t.Fatalf("GET /status?fmt=text: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	for _, want := range []string{
		"Install",
		"resolver:",
		"pool:",
		"Daemon",
		"running on 127.0.0.1:11616",
		"interface",
		"vip_cidr",
		"127.50.0.0/24",
		"dns_listen",
		"socks_listen",
		"Registered contexts",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text body missing %q\n\nfull body:\n%s", want, got)
		}
	}
}

func TestHandleStatus_NotReadyReportedAsStarting(t *testing.T) {
	s, _ := newStatusTestServer(t)
	// Replace with a not-ready handler.
	s.healthHandler = &health.Handler{}

	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/status?fmt=text", nil)
	s.handleStatus(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "starting") {
		t.Errorf("expected 'starting' indicator in non-ready text status; got:\n%s", body)
	}
}
