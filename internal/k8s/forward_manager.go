package k8s

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forwardEntry holds the TCP listener and the cancel func for its accept-loop goroutine.
type forwardEntry struct {
	listener net.Listener
	cancel   context.CancelFunc
}

// poolEntry is a cached SPDY connection shared across concurrent DialPortForward
// calls that target the same pod. refs counts the number of live streamConns
// backed by this connection; when it reaches zero the connection is closed and
// the entry is evicted from the pool.
type poolEntry struct {
	conn   httpstream.Connection
	nextID atomic.Int32 // monotonically incrementing request ID for stream pairs
	refs   atomic.Int32
}

// ForwardManager manages TCP listeners bound to virtual IP addresses. Each
// contextName/vip:svcPort triple gets one net.Listener. For every inbound TCP
// connection a fresh SPDY session is opened to the Kubernetes API server,
// targeting a randomly chosen pod endpoint, so load is distributed across all
// healthy pods on a per-connection basis.
//
// SPDY connections are pooled per (contextName, namespace, podName): multiple
// concurrent port-forwards to the same pod share one underlying TCP connection
// to the API server. Reference counting drives connection lifetime — the last
// streamConn to close triggers eviction.
type ForwardManager struct {
	clients  *ClientManager
	resolver *Resolver
	logger   *zap.SugaredLogger

	mu      sync.Mutex
	entries map[string]*forwardEntry // "contextName/vip:svcPort" -> entry

	spdyPoolMu sync.Mutex
	spdyPool   map[string]*poolEntry // "contextName/namespace/podName" -> entry
}

// NewForwardManager creates a new ForwardManager.
func NewForwardManager(clients *ClientManager, resolver *Resolver, logger *zap.SugaredLogger) *ForwardManager {
	return &ForwardManager{
		clients:  clients,
		resolver: resolver,
		logger:   logger,
		entries:  make(map[string]*forwardEntry),
		spdyPool: make(map[string]*poolEntry),
	}
}

// StartForward ensures a TCP listener is running on vipAddr:svcPort for the
// given Kubernetes service in contextName. When contextName is empty the
// current-context of the merged kubeconfig is used. Idempotent: a second call
// for the same contextName/vipAddr/svcPort is silently ignored.
//
// The listener is bound synchronously before StartForward returns, so callers
// can connect immediately. Each accepted TCP connection independently resolves
// a random pod endpoint and opens its own SPDY portforward session to that pod.
func (m *ForwardManager) StartForward(_ context.Context, contextName, vipAddr string, svcPort int32, namespace, svcName, podName string) error {
	if contextName == "" {
		contextName = m.clients.CurrentContextName()
	}
	key := contextName + "/" + fmt.Sprintf("%s:%d", vipAddr, svcPort)

	m.mu.Lock()
	if _, exists := m.entries[key]; exists {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", vipAddr, svcPort))
	if err != nil {
		return fmt.Errorf("listening on %s:%d: %w", vipAddr, svcPort, err)
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	entry := &forwardEntry{listener: ln, cancel: cancel}

	m.mu.Lock()
	// Double-check after re-acquiring: another goroutine may have raced us here.
	if _, exists := m.entries[key]; exists {
		m.mu.Unlock()
		cancel()
		_ = ln.Close()
		return nil
	}
	m.entries[key] = entry
	m.mu.Unlock()

	m.logger.Infow("port-forward listener bound",
		"vip", vipAddr,
		"svc_port", svcPort,
		"namespace", namespace,
		"service", svcName,
	)

	go m.listenLoop(loopCtx, ln, contextName, svcPort, namespace, svcName, podName)
	return nil
}

// listenLoop accepts TCP connections on ln until the listener is closed or ctx
// is cancelled, dispatching each to handleConn in its own goroutine.
func (m *ForwardManager) listenLoop(ctx context.Context, ln net.Listener, contextName string, svcPort int32, namespace, svcName, podName string) {
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // normal shutdown via Shutdown()
			}
			m.logger.Warnw("accept error on port-forward listener", "error", err)
			return
		}
		go m.handleConn(ctx, conn, contextName, svcPort, namespace, svcName, podName)
	}
}

// handleConn opens a port-forward stream to a pod backing the service and
// bidirectionally proxies conn over it for the connection's lifetime.
func (m *ForwardManager) handleConn(ctx context.Context, conn net.Conn, contextName string, svcPort int32, namespace, svcName, podName string) {
	defer func() {
		if err := conn.Close(); err != nil {
			m.logger.Warnw("closing accepted connection", "error", err)
		}
	}()

	pfConn, err := m.DialPortForward(ctx, contextName, namespace, svcName, podName, svcPort)
	if err != nil {
		m.logger.Errorw("port-forward dial failed",
			"namespace", namespace,
			"service", svcName,
			"error", err,
		)
		return
	}
	defer func() {
		if err := pfConn.Close(); err != nil {
			m.logger.Warnw("closing port-forward connection", "error", err)
		}
	}()

	// Bidirectional proxy between the accepted TCP connection and the port-forward
	// stream. When either side reaches EOF we half-close the write end of the
	// other side so it can drain and terminate cleanly, then wait for both
	// goroutines to finish before the deferred Close calls run.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := io.Copy(pfConn, conn); err != nil {
			m.logger.Warnw("copying to port-forward stream", "error", err)
		}
		if wc, ok := pfConn.(interface{ CloseWrite() error }); ok {
			_ = wc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(conn, pfConn); err != nil {
			m.logger.Warnw("copying from port-forward stream", "error", err)
		}
		if wc, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = wc.CloseWrite()
		}
	}()
	wg.Wait()
}

// acquirePoolEntry returns a *poolEntry with refs already incremented. It
// reuses a live cached connection for poolKey when one exists, or dials a
// fresh SPDY connection and stores it in the pool.
func (m *ForwardManager) acquirePoolEntry(poolKey string, upgrader spdy.Upgrader, transport http.RoundTripper, reqURL *url.URL) (*poolEntry, error) {
	m.spdyPoolMu.Lock()
	defer m.spdyPoolMu.Unlock()

	if e, exists := m.spdyPool[poolKey]; exists {
		// Check liveness without blocking: CloseChan is closed when the
		// underlying TCP connection has gone away.
		select {
		case <-e.conn.CloseChan():
			// Stale entry; fall through to dial a new connection.
			delete(m.spdyPool, poolKey)
		default:
			m.logger.Debugw("Reusing existing SPDY connection", "key", poolKey)
			e.refs.Add(1)
			return e, nil
		}
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)
	spdyConn, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("dialing SPDY portforward to pod: %w", err)
	}
	e := &poolEntry{conn: spdyConn}
	e.refs.Store(1)
	m.spdyPool[poolKey] = e
	m.logger.Debugw("Created new SPDY connection", "key", poolKey)
	return e, nil
}

// buildOnClose returns the ref-counting cleanup callback for a poolEntry.
// When the last streamConn backed by e is closed, the entry is evicted from
// the pool and the underlying SPDY connection is closed.
func (m *ForwardManager) buildOnClose(poolKey string, e *poolEntry) func() {
	return func() {
		if e.refs.Add(-1) != 0 {
			return
		}
		// Last reference dropped — evict and close, but only if this entry is
		// still the one in the pool (a concurrent dial may have replaced it)
		// and refs hasn't been bumped back up in the meantime.
		var shouldClose bool
		m.spdyPoolMu.Lock()
		if m.spdyPool[poolKey] == e && e.refs.Load() == 0 {
			delete(m.spdyPool, poolKey)
			shouldClose = true
		}
		m.spdyPoolMu.Unlock()
		if shouldClose {
			m.logger.Debugw("Closing SPDY connection", "key", poolKey)
			_ = e.conn.Close()
		}
	}
}

// DialPortForward resolves a random pod endpoint for the given service, opens a
// (possibly pooled) SPDY portforward stream to that pod, and returns a net.Conn
// backed by the SPDY data stream. The caller owns the returned connection and
// must close it when done; closing it decrements the pool reference count.
//
// SPDY connections are pooled per (contextName, namespace, podName) so that
// concurrent dials to the same pod share one underlying TCP connection to the
// API server rather than opening a new one for every call.
//
// This is a one-shot dial: unlike StartForward it does not create a persistent
// TCP listener and does not allocate a virtual IP. It is intended for callers
// (e.g. the SOCKS5 proxy) that already hold a client connection and want to
// stream bytes directly through the Kubernetes portforward API without any
// intermediate hop.
func (m *ForwardManager) DialPortForward(ctx context.Context, contextName, namespace, svcName, podName string, svcPort int32) (net.Conn, error) {
	if contextName == "" {
		contextName = m.clients.CurrentContextName()
	}

	cc, ok := m.clients.clientForContext(contextName)
	if !ok {
		return nil, fmt.Errorf("unknown kubeconfig context %q", contextName)
	}
	restConfig := cc.restConfig

	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resolved, err := m.resolver.Resolve(resolveCtx, contextName, namespace, svcName, podName)
	if err != nil {
		return nil, fmt.Errorf("resolving %s/%s: %w", namespace, svcName, err)
	}

	endpoint := m.resolver.PickEndpoint(resolved)
	podPort, err := m.resolver.FindPort(resolved, svcPort)
	if err != nil {
		return nil, fmt.Errorf("finding container port for svc port %d on %s/%s: %w", svcPort, namespace, svcName, err)
	}

	reqURL, err := url.Parse(restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing REST config host: %w", err)
	}
	reqURL.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward",
		url.PathEscape(endpoint.Namespace), url.PathEscape(endpoint.PodName))

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating SPDY round tripper: %w", err)
	}

	poolKey := contextName + "/" + endpoint.Namespace + "/" + endpoint.PodName
	portStr := fmt.Sprintf("%d", podPort)

	entry, err := m.acquirePoolEntry(poolKey, upgrader, transport, reqURL)
	if err != nil {
		return nil, err
	}

	streams, err := entry.createStreams(portStr)
	if err != nil {
		// CreateStream can fail on a connection that passed the CloseChan check
		// due to a TOCTOU gap: the server may have sent a GOAWAY, the TCP
		// connection may have been killed by a NAT timeout, or the API server
		// may have hit its idle-connection limit between our liveness check and
		// this call. Evict the stale entry and retry once with a fresh dial.
		m.spdyPoolMu.Lock()
		if m.spdyPool[poolKey] == entry {
			delete(m.spdyPool, poolKey)
		}
		m.spdyPoolMu.Unlock()
		_ = entry.conn.Close()

		entry, err = m.acquirePoolEntry(poolKey, upgrader, transport, reqURL)
		if err != nil {
			return nil, err
		}
		streams, err = entry.createStreams(portStr)
		if err != nil {
			m.buildOnClose(poolKey, entry)()
			return nil, err
		}
	}

	m.logger.Debugw("port-forward stream opened",
		"namespace", namespace,
		"pod", endpoint.PodName,
		"svc_port", svcPort,
		"pod_port", podPort,
	)

	return newStreamConn(streams.data, streams.error, nil, m.buildOnClose(poolKey, entry), m.logger, endpoint.PodName), nil
}

// Shutdown closes all active listeners, stops their accept loops, and closes
// all pooled SPDY connections.
func (m *ForwardManager) Shutdown() {
	m.mu.Lock()
	for key, entry := range m.entries {
		m.logger.Infow("stopping port-forward listener", "key", key)
		entry.cancel()
		_ = entry.listener.Close()
		delete(m.entries, key)
	}
	m.mu.Unlock()

	m.spdyPoolMu.Lock()
	for key, pe := range m.spdyPool {
		m.logger.Debugw("closing pooled SPDY connection", "key", key)
		_ = pe.conn.Close()
		delete(m.spdyPool, key)
	}
	m.spdyPoolMu.Unlock()
}

type portForwardStreams struct {
	data  httpstream.Stream
	error httpstream.Stream
}

// createStreams opens an error+data stream pair on the poolEntry and returns
// them. reqID is derived from nextID so concurrent callers get distinct IDs.
func (e *poolEntry) createStreams(portStr string) (portForwardStreams, error) {
	// nextID starts at 0 for the first stream on each connection and
	// increments so that concurrent forwards don't collide on the same ID.
	reqID := fmt.Sprintf("%d", e.nextID.Add(1)-1)

	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, portStr)
	headers.Set(corev1.PortForwardRequestIDHeader, reqID)
	errorStream, err := e.conn.CreateStream(headers)
	if err != nil {
		return portForwardStreams{}, fmt.Errorf("creating SPDY error stream to pod: %w", err)
	}

	headers = http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	headers.Set(corev1.PortHeader, portStr)
	headers.Set(corev1.PortForwardRequestIDHeader, reqID)
	dataStream, err := e.conn.CreateStream(headers)
	if err != nil {
		_ = errorStream.Reset()
		return portForwardStreams{}, fmt.Errorf("creating SPDY data stream to pod: %w", err)
	}

	return portForwardStreams{data: dataStream, error: errorStream}, nil
}
