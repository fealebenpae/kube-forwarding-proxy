// Package diagnostics holds best-effort startup checks that surface
// installation/configuration drift without blocking daemon startup.
package diagnostics

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// CheckResolverPort verifies that the macOS resolver file for clusterDomain
// declares the same port the daemon listens on (parsed from dnsListen).
//
// Returns the empty string when no warning is warranted:
//   - not running on darwin
//   - dnsListen is malformed
//   - the resolver file does not exist (privileged setup hasn't been run,
//     or the daemon runs in a context that doesn't use /etc/resolver/, e.g.
//     a compose sidecar)
//   - the resolver file's port matches dnsListen's port
//   - the resolver file is unreadable (best-effort; we don't surface I/O errors)
//
// Otherwise returns a one-line warning suitable for logging at WARN level,
// citing both ports and the fix command.
//
// macOS only: returns "" on every other GOOS.
func CheckResolverPort(clusterDomain, dnsListen string) string {
	return checkResolverPortAt("/etc/resolver", clusterDomain, dnsListen)
}

// checkResolverPortAt is the testable core: it accepts the resolver dir as a
// parameter so tests can point it at a temp dir.
func checkResolverPortAt(resolverDir, clusterDomain, dnsListen string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	daemonPort, ok := portFromAddr(dnsListen)
	if !ok {
		return ""
	}

	domain := strings.TrimSuffix(clusterDomain, ".")
	path := resolverDir + "/" + domain

	resolverPort, ok := readResolverPort(path)
	if !ok {
		return ""
	}
	if resolverPort == daemonPort {
		return ""
	}

	return fmt.Sprintf(
		"macOS resolver %s declares port %d but the daemon's DNS port is %d — "+
			"queries for *.%s will go to the wrong port. "+
			"Fix: ./k8s-service-proxy install --dns-port %d",
		path, resolverPort, daemonPort, domain, daemonPort)
}

// portFromAddr extracts the port from a "host:port" or ":port" listen string.
// Returns ok=false on any parse failure or zero/negative port.
func portFromAddr(addr string) (int, bool) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}

// readResolverPort returns the integer port from the first `port <N>` line in
// the named resolver file. ok=false when the file is missing, unreadable, or
// has no parseable port directive.
func readResolverPort(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "port" {
			if p, err := strconv.Atoi(fields[1]); err == nil && p > 0 {
				return p, true
			}
		}
	}
	return 0, false
}
