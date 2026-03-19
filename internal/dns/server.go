// Package dns provides an embedded DNS server that intercepts queries
// for Kubernetes service names and returns virtual IPs.
package dns

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/proxy"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/vip"
)

// Server is an embedded DNS server that intercepts Kubernetes service queries.
type Server struct {
	listenAddr     string
	clusterDomain  string
	allocator      *vip.Allocator
	resolver       *k8s.Resolver
	forwardManager *k8s.ForwardManager
	udpServer      *mdns.Server
	tcpServer      *mdns.Server
	udpConn        net.PacketConn
	tcpListener    net.Listener
	logger         *zap.SugaredLogger
}

// NewServer creates a new DNS server.
// Non-cluster DNS queries are forwarded to the nameservers from /etc/resolv.conf.
func NewServer(listenAddr, clusterDomain string, allocator *vip.Allocator, resolver *k8s.Resolver, forwardManager *k8s.ForwardManager, logger *zap.SugaredLogger) *Server {
	return &Server{
		listenAddr:     listenAddr,
		clusterDomain:  clusterDomain,
		allocator:      allocator,
		resolver:       resolver,
		forwardManager: forwardManager,
		logger:         logger,
	}
}

// Start begins listening on both UDP and TCP.
// The OS-assigned address is available via Addr() once Start returns.
func (s *Server) Start() error {
	mux := mdns.NewServeMux()
	mux.HandleFunc(s.clusterDomain+".", s.handleCluster)
	// Catch-all: handles context-suffixed cluster FQDNs and upstream forwarding.
	mux.HandleFunc(".", s.handleDefault)

	// Pre-bind both sockets so the OS-assigned port is known before we return.
	udpConn, err := net.ListenPacket("udp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("DNS UDP listen on %s: %w", s.listenAddr, err)
	}
	s.udpConn = udpConn

	tcpLn, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("DNS TCP listen on %s: %w", s.listenAddr, err)
	}
	s.tcpListener = tcpLn

	s.udpServer = &mdns.Server{
		PacketConn: udpConn,
		Net:        "udp",
		Handler:    mux,
	}

	s.tcpServer = &mdns.Server{
		Listener: tcpLn,
		Net:      "tcp",
		Handler:  mux,
	}

	errCh := make(chan error, 2)

	go func() {
		s.logger.Debugw("starting UDP server", "address", udpConn.LocalAddr().String())
		if err := s.udpServer.ActivateAndServe(); err != nil {
			errCh <- fmt.Errorf("UDP DNS server: %w", err)
		}
	}()

	go func() {
		s.logger.Debugw("starting TCP server", "address", tcpLn.Addr().String())
		if err := s.tcpServer.ActivateAndServe(); err != nil {
			errCh <- fmt.Errorf("TCP DNS server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

// Addr returns the address the DNS server is listening on (UDP).
// Only valid after Start() returns successfully.
func (s *Server) Addr() string {
	if s.udpConn == nil {
		return ""
	}
	return s.udpConn.LocalAddr().String()
}

// Shutdown gracefully stops the DNS server.
func (s *Server) Shutdown() {
	if s.udpServer != nil {
		if err := s.udpServer.Shutdown(); err != nil {
			s.logger.Errorw("UDP server shutdown error", "error", err)
		}
	}
	if s.tcpServer != nil {
		if err := s.tcpServer.Shutdown(); err != nil {
			s.logger.Errorw("TCP server shutdown error", "error", err)
		}
	}
}

// handleDefault is the catch-all DNS handler. It routes context-suffixed cluster
// FQDNs (e.g. svc.ns.svc.cluster.local.my-context.) to handleCluster and
// forwards everything else to the upstream resolvers in net.DefaultResolver.
func (s *Server) handleDefault(w mdns.ResponseWriter, r *mdns.Msg) {
	for _, q := range r.Question {
		s.logger.Debugw("DNS question",
			"name", q.Name,
			"type", mdns.TypeToString[q.Qtype],
		)
	}

	// If any A, TXT, or SRV question looks like a context-suffixed cluster FQDN,
	// route the whole message through the cluster handler (which tolerates unknown
	// names by returning NXDOMAIN per question).
	for _, q := range r.Question {
		if q.Qtype == mdns.TypeA || q.Qtype == mdns.TypeTXT || q.Qtype == mdns.TypeSRV {
			name := strings.TrimSuffix(q.Name, ".")
			_, _, _, ctxName, ok := proxy.ParseClusterHost(name, s.clusterDomain)
			if ok && ctxName != "" {
				s.handleCluster(w, r)
				return
			}
		}
	}
	s.forwardToUpstream(w, r)
}

// handleCluster handles DNS queries under the cluster domain (e.g. svc.cluster.local)
// as well as context-suffixed cluster FQDNs.
func (s *Server) handleCluster(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	hadTypeA := false
	aAnswerCount := 0
	for _, q := range r.Question {
		switch q.Qtype {
		case mdns.TypeA:
			hadTypeA = true
			for _, result := range s.resolveA(q.Name) {
				aAnswerCount++
				msg.Answer = append(msg.Answer, &mdns.A{
					Hdr: mdns.RR_Header{
						Name:   q.Name,
						Rrtype: mdns.TypeA,
						Class:  mdns.ClassINET,
						Ttl:    5,
					},
					A: result.ip,
				})
				msg.Extra = append(msg.Extra, &mdns.TXT{
					Hdr: mdns.RR_Header{
						Name:   q.Name,
						Rrtype: mdns.TypeTXT,
						Class:  mdns.ClassINET,
						Ttl:    5,
					},
					Txt: []string{fmt.Sprintf("ip=%s context=%s", result.ip.String(), result.contextName)},
				})
				// One SRV Extra record per exposed port, pointing at the
				// context-suffixed FQDN so resolvers can look up the exact VIP.
				srvTarget := s.contextFQDN(q.Name, result.contextName)
				for _, port := range result.ports {
					if port.Port == 0 {
						continue
					}
					msg.Extra = append(msg.Extra, &mdns.SRV{
						Hdr: mdns.RR_Header{
							Name:   q.Name,
							Rrtype: mdns.TypeSRV,
							Class:  mdns.ClassINET,
							Ttl:    5,
						},
						Priority: 0,
						Weight:   0,
						Port:     uint16(port.Port),
						Target:   srvTarget,
					})
				}
			}

		case mdns.TypeTXT:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			vips, serviceExists := s.lookupTXT(ctx, q.Name)
			cancel()
			if !serviceExists {
				msg.Rcode = mdns.RcodeNameError
			} else {
				for _, result := range vips {
					msg.Answer = append(msg.Answer, &mdns.TXT{
						Hdr: mdns.RR_Header{
							Name:   q.Name,
							Rrtype: mdns.TypeTXT,
							Class:  mdns.ClassINET,
							Ttl:    5,
						},
						Txt: []string{fmt.Sprintf("ip=%s context=%s", result.ip.String(), result.contextName)},
					})
				}
			}

		case mdns.TypeSRV:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			vips, serviceExists := s.lookupSRV(ctx, q.Name)
			cancel()
			if !serviceExists {
				msg.Rcode = mdns.RcodeNameError
			} else {
				for _, result := range vips {
					srvTarget := s.contextFQDN(q.Name, result.contextName)
					for _, port := range result.ports {
						if port.Port == 0 {
							continue
						}
						msg.Answer = append(msg.Answer, &mdns.SRV{
							Hdr: mdns.RR_Header{
								Name:   q.Name,
								Rrtype: mdns.TypeSRV,
								Class:  mdns.ClassINET,
								Ttl:    5,
							},
							Priority: 0,
							Weight:   0,
							Port:     uint16(port.Port),
							Target:   srvTarget,
						})
					}
				}
			}
		}
	}

	// Only set NXDOMAIN when there were TypeA questions and none resolved.
	// For non-A queries (e.g. AAAA) return NOERROR with empty answers (NODATA)
	// so clients don't cache the name as non-existent and still try TypeA.
	// (TypeTXT sets its own Rcode directly above when needed.)
	if hadTypeA && aAnswerCount == 0 {
		msg.Rcode = mdns.RcodeNameError
	}

	if err := w.WriteMsg(msg); err != nil {
		s.logger.Errorw("failed to write cluster response", "error", err)
	}
}

// contextVIP pairs an allocated virtual IP with the kubeconfig context it belongs to.
// Ports is populated from the Kubernetes Service definition and lists every port
// the service exposes; it is used to generate SRV records.
type contextVIP struct {
	contextName string
	ip          net.IP
	ports       []k8s.ServicePort
}

// resolveForContext allocates (or returns the cached) VIP for a service in one
// kubeconfig context and ensures TCP listeners are running for all service ports.
// Returns nil when the service cannot be resolved or the VIP cannot be allocated.
func (s *Server) resolveForContext(ctx context.Context, contextName, namespace, svcName, podName string) *contextVIP {
	key := contextName + "/" + namespace + "/" + svcName
	if podName != "" {
		key += "/" + podName
	}

	if ip := s.allocator.Lookup(key); ip != nil {
		// VIP already allocated — re-resolve to get current port list (resolver
		// uses its own cache so this is nearly free).
		var ports []k8s.ServicePort
		if resolved, err := s.resolver.Resolve(ctx, contextName, namespace, svcName, podName); err == nil {
			ports = resolved.Ports
		}
		return &contextVIP{contextName: contextName, ip: ip, ports: ports}
	}

	resolved, err := s.resolver.Resolve(ctx, contextName, namespace, svcName, podName)
	if err != nil {
		s.logger.Debugw("failed to resolve service in context",
			"context", contextName,
			"key", key,
			"error", err)
		return nil
	}

	vipAddr, err := s.allocator.Allocate(key)
	if err != nil {
		s.logger.Errorw("failed to allocate VIP", "key", key, "error", err)
		return nil
	}

	s.logger.Infow("allocated VIP",
		"vip", vipAddr.String(),
		"context", contextName,
		"key", key)

	for _, port := range resolved.Ports {
		if err := s.forwardManager.StartForward(ctx, contextName, vipAddr.String(), port.Port, namespace, svcName, podName); err != nil {
			s.logger.Errorw("failed to start forward",
				"key", key,
				"port", port.Port,
				"error", err)
			return nil
		}
	}

	return &contextVIP{contextName: contextName, ip: vipAddr, ports: resolved.Ports}
}

// resolveA resolves A records for a Kubernetes service or pod FQDN.
// When a context name is encoded in the hostname only that context is queried;
// otherwise all known kubeconfig contexts are tried and every successful result
// is returned (one VIP per context that has the service).
func (s *Server) resolveA(name string) []contextVIP {
	name = strings.TrimSuffix(name, ".")

	podName, svcName, namespace, contextName, ok := proxy.ParseClusterHost(name, s.clusterDomain)
	if !ok {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if contextName != "" {
		result := s.resolveForContext(ctx, contextName, namespace, svcName, podName)
		if result == nil {
			return nil
		}
		return []contextVIP{*result}
	}

	// No context specified: try every known kubeconfig context.
	var results []contextVIP
	for _, ctxName := range s.resolver.AllContextNames() {
		if r := s.resolveForContext(ctx, ctxName, namespace, svcName, podName); r != nil {
			results = append(results, *r)
		}
	}
	return results
}

// contextFQDN returns the context-suffixed FQDN for q.Name (with trailing dot
// already present) in the given kubeconfig context. For a bare cluster name
// (no existing context suffix) it appends ".<contextName>." to the stripped
// base name. For a name that already has a context suffix the existing suffix is
// replaced.
//
// Example: contextFQDN("svc.ns.svc.cluster.local.", "ctx-a") →
//
//	"svc.ns.svc.cluster.local.ctx-a."
func (s *Server) contextFQDN(qname, contextName string) string {
	base := strings.TrimSuffix(qname, ".")
	// Strip any existing context suffix so we always produce a canonical result.
	if _, _, _, existingCtx, ok := proxy.ParseClusterHost(base, s.clusterDomain); ok && existingCtx != "" {
		// Remove the trailing ".<ctx>" portion.
		base = base[:len(base)-len(existingCtx)-1]
	}
	return base + "." + contextName + "."
}

// lookupTXT is the informative-only counterpart of resolveA: it never allocates
// VIPs or starts forwarders.
//
// First it checks whether any VIPs have already been allocated for the hostname
// (across the relevant context set). If so, it returns those VIPs immediately
// with serviceExists=true — no Kubernetes API calls are made.
//
// If no VIPs are found it performs a lightweight existence check: it calls
// resolver.Resolve for each relevant context and sets serviceExists=true on the
// first success, then returns immediately. This lets the TypeTXT handler
// distinguish NXDOMAIN (service not found anywhere) from NODATA (service exists
// but no VIPs have been allocated yet).
func (s *Server) lookupTXT(ctx context.Context, name string) (vips []contextVIP, serviceExists bool) {
	name = strings.TrimSuffix(name, ".")

	podName, svcName, namespace, contextName, ok := proxy.ParseClusterHost(name, s.clusterDomain)
	if !ok {
		return nil, false
	}

	contexts := s.resolver.AllContextNames()
	if contextName != "" {
		contexts = []string{contextName}
	}

	// First pass: return already-allocated VIPs from cache — no API calls.
	for _, ctxName := range contexts {
		key := ctxName + "/" + namespace + "/" + svcName
		if podName != "" {
			key += "/" + podName
		}
		if ip := s.allocator.Lookup(key); ip != nil {
			vips = append(vips, contextVIP{contextName: ctxName, ip: ip})
		}
	}
	if len(vips) > 0 {
		return vips, true
	}

	// Second pass: existence check only — break on first successful resolve.
	for _, ctxName := range contexts {
		if _, err := s.resolver.Resolve(ctx, ctxName, namespace, svcName, podName); err == nil {
			return nil, true
		}
	}
	return nil, false
}

// lookupSRV is the informative-only SRV lookup: it never allocates VIPs or
// starts forwarders. Its NXDOMAIN/NODATA semantics are identical to lookupTXT.
//
// First it checks whether any VIPs have already been allocated for the hostname.
// For each allocated VIP it calls resolver.Resolve (cached) to obtain the port
// list, then returns the full []contextVIP with ports populated.
//
// If no VIPs exist it performs a lightweight existence check and sets
// serviceExists=true on the first successful resolve (NODATA). If no context
// can resolve the service, serviceExists=false (NXDOMAIN).
func (s *Server) lookupSRV(ctx context.Context, name string) (vips []contextVIP, serviceExists bool) {
	name = strings.TrimSuffix(name, ".")

	podName, svcName, namespace, contextName, ok := proxy.ParseClusterHost(name, s.clusterDomain)
	if !ok {
		return nil, false
	}

	contexts := s.resolver.AllContextNames()
	if contextName != "" {
		contexts = []string{contextName}
	}

	// First pass: return already-allocated VIPs with port info — no allocation.
	for _, ctxName := range contexts {
		key := ctxName + "/" + namespace + "/" + svcName
		if podName != "" {
			key += "/" + podName
		}
		if ip := s.allocator.Lookup(key); ip != nil {
			var ports []k8s.ServicePort
			if resolved, err := s.resolver.Resolve(ctx, ctxName, namespace, svcName, podName); err == nil {
				ports = resolved.Ports
			}
			vips = append(vips, contextVIP{contextName: ctxName, ip: ip, ports: ports})
		}
	}
	if len(vips) > 0 {
		return vips, true
	}

	// Second pass: existence check only — break on first successful resolve.
	for _, ctxName := range contexts {
		if _, err := s.resolver.Resolve(ctx, ctxName, namespace, svcName, podName); err == nil {
			return nil, true
		}
	}
	return nil, false
}

// forwardToUpstream resolves a DNS query via net.DefaultResolver and writes the response.
func (s *Server) forwardToUpstream(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.RecursionAvailable = true

	for _, q := range r.Question {
		name := strings.TrimSuffix(q.Name, ".")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		switch q.Qtype {
		case mdns.TypeA:
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream A lookup failed", "name", name, "error", err)
			}
			for _, addr := range addrs {
				if ip4 := addr.IP.To4(); ip4 != nil {
					msg.Answer = append(msg.Answer, &mdns.A{
						Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
						A:   ip4,
					})
				}
			}
		case mdns.TypeAAAA:
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream AAAA lookup failed", "name", name, "error", err)
			}
			for _, addr := range addrs {
				if addr.IP.To4() == nil {
					msg.Answer = append(msg.Answer, &mdns.AAAA{
						Hdr:  mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeAAAA, Class: mdns.ClassINET, Ttl: 60},
						AAAA: addr.IP,
					})
				}
			}
		case mdns.TypeCNAME:
			cname, err := net.DefaultResolver.LookupCNAME(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream CNAME lookup failed", "name", name, "error", err)
			} else {
				msg.Answer = append(msg.Answer, &mdns.CNAME{
					Hdr:    mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeCNAME, Class: mdns.ClassINET, Ttl: 60},
					Target: cname,
				})
			}
		case mdns.TypeMX:
			mxs, err := net.DefaultResolver.LookupMX(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream MX lookup failed", "name", name, "error", err)
			}
			for _, mx := range mxs {
				msg.Answer = append(msg.Answer, &mdns.MX{
					Hdr:        mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeMX, Class: mdns.ClassINET, Ttl: 60},
					Preference: mx.Pref,
					Mx:         mx.Host,
				})
			}
		case mdns.TypeTXT:
			txts, err := net.DefaultResolver.LookupTXT(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream TXT lookup failed", "name", name, "error", err)
			} else {
				msg.Answer = append(msg.Answer, &mdns.TXT{
					Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeTXT, Class: mdns.ClassINET, Ttl: 60},
					Txt: txts,
				})
			}
		case mdns.TypeNS:
			nss, err := net.DefaultResolver.LookupNS(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream NS lookup failed", "name", name, "error", err)
			}
			for _, ns := range nss {
				msg.Answer = append(msg.Answer, &mdns.NS{
					Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeNS, Class: mdns.ClassINET, Ttl: 60},
					Ns:  ns.Host,
				})
			}
		case mdns.TypePTR:
			ptrs, err := net.DefaultResolver.LookupAddr(ctx, name)
			if err != nil {
				s.logger.Warnw("upstream PTR lookup failed", "name", name, "error", err)
			}
			for _, ptr := range ptrs {
				msg.Answer = append(msg.Answer, &mdns.PTR{
					Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypePTR, Class: mdns.ClassINET, Ttl: 60},
					Ptr: ptr,
				})
			}
		default:
			s.logger.Warnw("unsupported query type for upstream resolution", "name", name, "type", mdns.TypeToString[q.Qtype])
		}

		cancel()
	}

	if err := w.WriteMsg(msg); err != nil {
		s.logger.Errorw("failed to write upstream response", "error", err)
	}
}
