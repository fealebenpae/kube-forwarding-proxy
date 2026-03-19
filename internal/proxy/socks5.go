package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/armon/go-socks5"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
)

// SOCKS5Proxy is a SOCKS5 server that tunnels connections to Kubernetes
// services via port-forward, and passes non-cluster traffic through directly.
type SOCKS5Proxy struct {
	listenAddr     string
	clusterDomain  string
	forwardManager *k8s.ForwardManager
	server         *socks5.Server
	listener       net.Listener
	logger         *zap.SugaredLogger
}

// NewSOCKS5Proxy creates a new SOCKS5 proxy.
func NewSOCKS5Proxy(listenAddr, clusterDomain string, forwardManager *k8s.ForwardManager, logger *zap.SugaredLogger) (*SOCKS5Proxy, error) {
	p := &SOCKS5Proxy{
		listenAddr:     listenAddr,
		clusterDomain:  clusterDomain,
		forwardManager: forwardManager,
		logger:         logger,
	}

	// Bridge the armon/go-socks5 library's internal logger to zap.
	socksLog, _ := zap.NewStdLogAt(logger.Desugar().Named("go-socks5"), zapcore.DebugLevel)

	conf := &socks5.Config{
		Logger: socksLog,
		Dial:   p.dial,
		// clusterAwareResolver short-circuits cluster hostnames so that
		// armon/go-socks5 always proceeds to the custom Dial step rather than
		// failing when system DNS can't resolve *.svc.cluster.local names.
		Resolver: &clusterAwareResolver{clusterDomain: clusterDomain},
	}

	srv, err := socks5.New(conf)
	if err != nil {
		return nil, fmt.Errorf("creating SOCKS5 server: %w", err)
	}
	p.server = srv

	return p, nil
}

// Start begins accepting SOCKS5 connections.
func (p *SOCKS5Proxy) Start() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		p.logger.Errorw("listen error", "address", p.listenAddr, "error", err)
		return fmt.Errorf("SOCKS5 listen on %s: %w", p.listenAddr, err)
	}
	p.listener = ln

	p.logger.Debugw("listening", "address", p.listener.Addr().String())

	go func() {
		if err := p.server.Serve(ln); err != nil {
			// Serve returns an error when the listener is closed during shutdown.
			if p.listener != nil {
				p.logger.Errorw("server error", "error", err)
			}
		}
	}()

	return nil
}

// Addr returns the address the SOCKS5 proxy is listening on.
// Only valid after Start() returns successfully.
func (p *SOCKS5Proxy) Addr() string {
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// Shutdown stops the SOCKS5 proxy.
func (p *SOCKS5Proxy) Shutdown() {
	if p.listener != nil {
		ln := p.listener
		p.listener = nil
		if err := ln.Close(); err != nil {
			p.logger.Warnw("error closing listener", "error", err)
		}
	}
}

// dial is the custom dialer for the SOCKS5 server. It routes cluster
// destinations through the K8s port-forward API and dials everything
// else directly.
func (p *SOCKS5Proxy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("parsing address %q: %w", addr, err)
	}

	if IsClusterHost(host, p.clusterDomain) {
		return p.dialCluster(ctx, host, portStr)
	}

	// Non-cluster destination: direct dial.
	p.logger.Debugw("direct dial", "address", addr)
	return net.DialTimeout(network, addr, 10*time.Second)
}

// dialCluster resolves a cluster service (or pod within a headless service) name
// and returns a net.Conn backed directly by a SPDY portforward stream to the
// selected pod. No virtual IP address or intermediate TCP listener is involved.
func (p *SOCKS5Proxy) dialCluster(ctx context.Context, host, portStr string) (net.Conn, error) {
	host = strings.TrimSuffix(host, ".")

	podName, svcName, namespace, contextName, ok := ParseClusterHost(host, p.clusterDomain)
	if !ok {
		return nil, fmt.Errorf("invalid cluster service name: %s", host)
	}

	var svcPort int32
	if _, err := fmt.Sscanf(portStr, "%d", &svcPort); err != nil {
		return nil, fmt.Errorf("invalid port %q: %w", portStr, err)
	}

	p.logger.Infow("tunnel via port-forward stream",
		"namespace", namespace,
		"service", svcName,
		"pod", podName,
		"port", svcPort,
	)

	return p.forwardManager.DialPortForward(ctx, contextName, namespace, svcName, podName, svcPort)
}

// clusterAwareResolver implements socks5.NameResolver. For cluster-domain
// hostnames it returns nil IP so that armon/go-socks5's AddrSpec.Address()
// falls back to the FQDN when calling Dial, allowing the custom Dial function
// to route the connection via port-forward. All other hostnames are resolved
// via the system DNS resolver.
type clusterAwareResolver struct {
	clusterDomain string
}

func (r *clusterAwareResolver) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	if IsClusterHost(name, r.clusterDomain) {
		return ctx, nil, nil
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		return ctx, nil, err
	}
	return ctx, net.ParseIP(addrs[0]), nil
}
