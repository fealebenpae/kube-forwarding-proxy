// Package install implements the privileged macOS host setup that lets the
// daemon run unprivileged: writing /etc/resolver/<cluster-domain> and
// pre-aliasing a /24 of loopback VIPs onto lo0.
//
// Platform-agnostic helpers (resolver-file content rendering, CIDR pool
// expansion) live in this file. Darwin-only I/O — file writes under /etc,
// `ifconfig` invocations — lives in install_darwin.go.
package install

import (
	"fmt"
	"net"
	"strings"
)

// Defaults for Options. Zero-valued fields are filled with these by WithDefaults.
const (
	DefaultClusterDomain = "svc.cluster.local"
	DefaultDNSPort       = 11617
	DefaultPoolCIDR      = "127.50.0.0/24"
	DefaultPoolSize      = 255
	DefaultLoopbackIface = "lo0"
	DefaultResolverDir   = "/etc/resolver"
)

// Options are inputs to Install/Uninstall. Zero values are filled with the
// defaults above so callers can leave any field unset.
type Options struct {
	ClusterDomain string // e.g. "svc.cluster.local"
	DNSPort       int    // port written into the resolver file (= daemon's DNSListen port)
	PoolCIDR      string // e.g. "127.50.0.0/24"
	PoolSize      int    // number of IPs to alias from the start of the CIDR (1..255)
	LoopbackIface string // e.g. "lo0"
	ResolverDir   string // e.g. "/etc/resolver"
}

// WithDefaults returns o with any zero-valued field replaced by its default.
func (o Options) WithDefaults() Options {
	if o.ClusterDomain == "" {
		o.ClusterDomain = DefaultClusterDomain
	}
	if o.DNSPort == 0 {
		o.DNSPort = DefaultDNSPort
	}
	if o.PoolCIDR == "" {
		o.PoolCIDR = DefaultPoolCIDR
	}
	if o.PoolSize == 0 {
		o.PoolSize = DefaultPoolSize
	}
	if o.LoopbackIface == "" {
		o.LoopbackIface = DefaultLoopbackIface
	}
	if o.ResolverDir == "" {
		o.ResolverDir = DefaultResolverDir
	}
	return o
}

// Validate checks the options' invariants. Returns a descriptive error on
// any malformed input.
func (o Options) Validate() error {
	if o.ClusterDomain == "" {
		return fmt.Errorf("ClusterDomain must be non-empty")
	}
	if o.DNSPort <= 0 || o.DNSPort > 65535 {
		return fmt.Errorf("DNSPort must be in 1..65535 (got %d)", o.DNSPort)
	}
	if o.PoolSize < 1 || o.PoolSize > 255 {
		return fmt.Errorf("PoolSize must be in 1..255 (got %d)", o.PoolSize)
	}
	if _, err := PoolIPs(o.PoolCIDR, o.PoolSize); err != nil {
		return err
	}
	return nil
}

// ResolverPath is the absolute path to the per-domain resolver file under
// /etc/resolver/. The trailing dot on a fully-qualified domain is stripped.
func (o Options) ResolverPath() string {
	return o.ResolverDir + "/" + strings.TrimSuffix(o.ClusterDomain, ".")
}

// ResolverBody is the byte content the resolver file should hold: two lines
// plus a trailing newline so the idempotency check is a strict equality test.
func (o Options) ResolverBody() string {
	return fmt.Sprintf("nameserver 127.0.0.1\nport %d\n", o.DNSPort)
}

// PoolIPs expands cidr (must be an IPv4 /24 of the form A.B.C.0/24) into the
// first `size` host addresses (A.B.C.1 .. A.B.C.size). size must be in 1..255.
func PoolIPs(cidr string, size int) ([]net.IP, error) {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 24 {
		return nil, fmt.Errorf("CIDR must be an IPv4 /24 (got %s)", cidr)
	}
	if size < 1 || size > 255 {
		return nil, fmt.Errorf("size must be in 1..255 (got %d)", size)
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil, fmt.Errorf("CIDR is not IPv4 (got %s)", cidr)
	}
	if v4[3] != 0 {
		return nil, fmt.Errorf("CIDR base must end in .0 (got %s)", cidr)
	}
	out := make([]net.IP, size)
	for i := 0; i < size; i++ {
		out[i] = net.IPv4(v4[0], v4[1], v4[2], byte(i+1)).To4()
	}
	return out, nil
}
