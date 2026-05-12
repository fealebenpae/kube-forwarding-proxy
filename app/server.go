// Package app provides the high-level Server that wires all proxy components
// together and is shared by cmd/proxy and the e2e test suite.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/api"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/diagnostics"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/dns"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/health"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/install"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/proxy"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/vip"
)

// Server owns the full lifecycle of the proxy: HTTP health endpoint, optional
// DNS server, optional SOCKS5 proxy, VIP allocator, and Kubernetes client
// manager. Construct it with NewServer, call Start to bind all listeners, and
// call Stop to shut everything down cleanly.
type Server struct {
	cfg         Config
	logger      *zap.SugaredLogger
	enableDNS   bool
	enableSocks bool

	// startTime is set in NewServer; used by /status to report uptime in
	// the same `[[dd-]hh:]mm:ss` shape as `ps etime=`.
	startTime time.Time

	// healthHandler is retained so /status can report the daemon's ready bit
	// directly (the readyz handler flips ready once Start() finishes).
	healthHandler *health.Handler

	// Populated after Start() returns successfully.
	HTTPAddr  string // address the HTTP health server is listening on
	DNSAddr   string // address the DNS server is listening on; empty when DNS is disabled
	SOCKSAddr string // address the SOCKS5 proxy is listening on; empty when SOCKS is disabled

	// internal components, kept for Stop()
	httpSrv        *http.Server
	k8sManager     *k8s.ClientManager
	allocator      *vip.Allocator
	forwardManager *k8s.ForwardManager
	dnsServer      *dns.Server
	socks5Proxy    *proxy.SOCKS5Proxy
}

// NewServer creates a Server from the given configuration.
// enableDNS and enableSocks control which subsystems are started by Start.
func NewServer(cfg Config, logger *zap.SugaredLogger, enableDNS, enableSocks bool) *Server {
	return &Server{
		cfg:         cfg,
		logger:      logger,
		enableDNS:   enableDNS,
		enableSocks: enableSocks,
		startTime:   time.Now(),
	}
}

// Start binds all listeners and starts all background goroutines.
// On return the HTTPAddr, DNSAddr, and SOCKSAddr fields are populated with the
// actual listening addresses chosen by the OS.
func (s *Server) Start() error {
	_, ifaceName, err := vip.ResolveIface(s.cfg.Interface)
	if err != nil {
		return fmt.Errorf("resolving INTERFACE %q: %w", s.cfg.Interface, err)
	}

	// HTTP health / kubeconfig endpoint.
	mux := http.NewServeMux()
	s.healthHandler = health.AddToMux(mux)
	kubeconfigHandler := k8s.AddToMux(mux)
	mux.HandleFunc("/status", s.handleStatus)

	httpLn, err := net.Listen("tcp", s.cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("HTTP listen on %s: %w", s.cfg.HTTPListen, err)
	}
	s.httpSrv = &http.Server{
		Addr:         httpLn.Addr().String(),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	s.HTTPAddr = httpLn.Addr().String()
	go func() { _ = s.httpSrv.Serve(httpLn) }()
	s.logger.Debugw("HTTP server listening", "address", s.HTTPAddr)

	// Kubernetes client manager.
	s.k8sManager, err = k8s.NewClientManager(s.logger.Named("k8s"))
	if err != nil {
		return fmt.Errorf("initialising kubernetes client manager: %w", err)
	}

	// VIP allocator — adds/removes IP aliases on the configured interface.
	addressManager, err := vip.NewInterfaceAddressManager(ifaceName, s.cfg.VIPAliasMode)
	if err != nil {
		return fmt.Errorf("creating interface address manager: %w", err)
	}
	s.allocator, err = vip.NewAllocator(addressManager, s.cfg.VIPCIDR)
	if err != nil {
		return fmt.Errorf("creating VIP allocator: %w", err)
	}

	resolver := k8s.NewResolver(s.k8sManager, 15*time.Second)
	s.forwardManager = k8s.NewForwardManager(s.k8sManager, resolver, s.logger.Named("forward"), s.allocator, s.cfg.VIPIdleTimeout)

	// Wire kubeconfig HTTP handler to the live managers.
	kubeconfigHandler.SetManagers(s.k8sManager, s.forwardManager)

	if s.enableDNS {
		if msg := diagnostics.CheckResolverPort(s.cfg.ClusterDomain, s.cfg.DNSListen); msg != "" {
			s.logger.Warn(msg)
		}

		s.dnsServer = dns.NewServer(
			s.cfg.DNSListen,
			s.cfg.ClusterDomain,
			s.allocator,
			resolver,
			s.forwardManager,
			s.logger.Named("dns"),
		)
		if err := s.dnsServer.Start(); err != nil {
			return fmt.Errorf("starting DNS server: %w", err)
		}
		s.DNSAddr = s.dnsServer.Addr()
	}

	if s.enableSocks {
		s.socks5Proxy, err = proxy.NewSOCKS5Proxy(
			s.cfg.SOCKSListen,
			s.cfg.ClusterDomain,
			s.forwardManager,
			s.logger.Named("socks5"),
		)
		if err != nil {
			return fmt.Errorf("creating SOCKS5 proxy: %w", err)
		}
		if err := s.socks5Proxy.Start(); err != nil {
			return fmt.Errorf("starting SOCKS5 proxy: %w", err)
		}
		s.SOCKSAddr = s.socks5Proxy.Addr()
	}

	s.healthHandler.SetReady()
	s.logger.Infow("proxy ready",
		"http", s.HTTPAddr,
		"dns", s.DNSAddr,
		"socks5", s.SOCKSAddr,
	)
	return nil
}

// handleStatus returns the daemon's full runtime status. The default
// representation is JSON (install state, daemon settings, registered
// contexts + per-context proxy reachability). `?fmt=text` returns the same
// data rendered as the human-readable report the `status` subcommand
// prints. Read-only; safe to expose alongside /healthz and /readyz.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	settings := api.StatusResponse{
		Interface:      s.cfg.Interface,
		VIPCIDR:        s.cfg.VIPCIDR,
		VIPAliasMode:   s.cfg.VIPAliasMode,
		VIPIdleTimeout: s.cfg.VIPIdleTimeout.String(),
		ClusterDomain:  s.cfg.ClusterDomain,
		LogLevel:       s.cfg.LogLevel,
		HTTPListen:     s.HTTPAddr,
		DNSEnabled:     s.enableDNS,
		DNSListen:      s.DNSAddr,
		SOCKSEnabled:   s.enableSocks,
		SOCKSListen:    s.SOCKSAddr,
	}

	internals := install.ServerInternals{
		HTTPAddr:  s.HTTPAddr,
		Settings:  &settings,
		StartTime: s.startTime,
		Ready:     s.healthHandler.IsReady(),
		PID:       os.Getpid(),
		Command:   strings.Join(os.Args, " "),
	}
	if s.k8sManager != nil {
		internals.Kubeconfig = s.k8sManager.Kubeconfig()
		internals.CurrentContext = s.k8sManager.CurrentContextName()
	}

	opts := installOptionsFromConfig(s.cfg)
	status := install.ComputeStatusForServer(opts, internals)

	if r.URL.Query().Get("fmt") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		status.Print(w, opts)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

// installOptionsFromConfig builds an install.Options describing the install
// state expected to back this daemon's configuration. Pool size and
// resolver dir fall back to install defaults — those aren't tracked in
// app.Config and the status comparison is best-effort either way.
func installOptionsFromConfig(cfg Config) install.Options {
	opts := install.Options{
		ClusterDomain: cfg.ClusterDomain,
		LoopbackIface: cfg.Interface,
		PoolCIDR:      cfg.VIPCIDR,
	}
	if _, port, err := net.SplitHostPort(cfg.DNSListen); err == nil {
		if p, err := strconv.Atoi(port); err == nil && p > 0 {
			opts.DNSPort = p
		}
	}
	return opts.WithDefaults()
}

// Stop shuts down all components gracefully within the given context deadline.
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(ctx)
	}
	if s.socks5Proxy != nil {
		s.socks5Proxy.Shutdown()
	}
	if s.dnsServer != nil {
		s.dnsServer.Shutdown()
	}
	if s.forwardManager != nil {
		s.forwardManager.Shutdown()
	}
	if s.allocator != nil {
		if err := s.allocator.Cleanup(); err != nil {
			return fmt.Errorf("VIP cleanup: %w", err)
		}
	}
	return nil
}
