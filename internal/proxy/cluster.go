// Package proxy implements SOCKS5 proxying for Kubernetes cluster services.
package proxy

import "strings"

// IsClusterHost reports whether host falls under the given cluster domain.
// It handles both bare hostnames (e.g. svc.ns.svc.cluster.local) and
// context-suffixed ones (e.g. svc.ns.svc.cluster.local.my-context).
func IsClusterHost(host, clusterDomain string) bool {
	domain := "." + clusterDomain
	if strings.HasSuffix(host, domain) || strings.HasSuffix(host, domain+".") {
		return true
	}
	// context-suffixed: <labels>.svc.cluster.local.<context> (single label, no dots)
	idx := strings.Index(host, domain+".")
	if idx >= 0 {
		rest := host[idx+len(domain)+1:]
		return rest != "" && !strings.Contains(rest, ".")
	}
	return false
}

// ParseClusterHost parses a Kubernetes service or pod FQDN under clusterDomain
// into its constituent parts. The trailing dot, if present, must be stripped by
// the caller before passing host.
//
// Accepted formats (bare):
//
//	<service>.<namespace>.svc.cluster.local       -> contextName = ""
//	<pod>.<service>.<namespace>.svc.cluster.local -> contextName = ""
//
// Accepted formats (context-suffixed, routes to a specific kubeconfig context):
//
//	<service>.<namespace>.svc.cluster.local.<ctx>       -> contextName = "<ctx>"
//	<pod>.<service>.<namespace>.svc.cluster.local.<ctx> -> contextName = "<ctx>"
func ParseClusterHost(host, clusterDomain string) (podName, svcName, namespace, contextName string, ok bool) {
	suffix := "." + clusterDomain

	var prefix string
	if before, ok := strings.CutSuffix(host, suffix); ok {
		// Bare form: ends exactly with the cluster domain.
		prefix = before
		contextName = ""
	} else {
		// Context-suffixed form: <prefix>.svc.cluster.local.<context>
		idx := strings.LastIndex(host, suffix+".")
		if idx < 0 {
			return "", "", "", "", false
		}
		after := host[idx+len(suffix)+1:]
		if after == "" || strings.Contains(after, ".") {
			// Empty context or context with dots — not supported.
			return "", "", "", "", false
		}
		contextName = after
		prefix = host[:idx]
	}

	parts := strings.Split(prefix, ".")
	switch len(parts) {
	case 2:
		// <service>.<namespace>
		return "", parts[0], parts[1], contextName, true
	case 3:
		// <pod>.<service>.<namespace>
		return parts[0], parts[1], parts[2], contextName, true
	default:
		return "", "", "", "", false
	}
}
