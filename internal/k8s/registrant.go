package k8s

import (
	"net"
	netUrl "net/url"
	"sort"
	"strings"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DerivePortSuffix returns the port from a cluster's proxy-url, or "" when
// no proxy-url is set or the URL is unparseable. Used by RewriteForRegistrant
// to suffix cluster, context, and auth-info names so different worktrees —
// each with its own SOCKS/HTTP-CONNECT tunnel on a distinct local port —
// stay independently addressable even when their kubeconfig context names
// happen to match (e.g. every kind cluster's context is "kind-kind").
//
// Identity is `(original-name, proxy-port)`. Re-registering the same tuple
// PATCH-overwrites the previous entry — that's the eviction story when a
// kind cluster gets recreated under the same tunnel: same name+port, fresh
// CA, single entry. Different ports keep both entries because they
// represent different tunnels (and therefore different upstream hosts).
//
// When proxy-url has no explicit port the scheme's default is used:
// http→80, https→443, socks5/socks5h→1080.
//
// Exported so the `register` subcommand can preview the rename without
// re-parsing the daemon's response.
func DerivePortSuffix(cluster *clientcmdapi.Cluster) string {
	if cluster == nil || cluster.ProxyURL == "" {
		return ""
	}
	u, err := netUrl.Parse(cluster.ProxyURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(u.Host); err == nil && port != "" {
		return port
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "socks5", "socks5h":
		return "1080"
	case "http":
		return "80"
	default:
		return ""
	}
}

// withSuffix returns name+"-"+id, unless id is empty (no rewrite) or name
// already ends with that suffix (idempotent).
func withSuffix(id, name string) string {
	if id == "" {
		return name
	}
	s := "-" + id
	if strings.HasSuffix(name, s) {
		return name
	}
	return name + s
}

// RewriteForRegistrant returns a copy of cfg with cluster, context, and
// auth-info names suffixed by their cluster's proxy-url port. Cross-references
// inside contexts are rewritten to the new names; CurrentContext is rewritten
// too.
//
// The rewrite is the daemon's safeguard against name collisions when
// multiple registrants (different worktrees, parallel devcontainers) PATCH
// kubeconfigs whose context names happen to be identical. Every worktree
// gets its own tunnel port → its own suffix → independently addressable.
// Re-registering the same context name on the same tunnel overwrites the
// previous entry in place (built into kubeconfig PATCH semantics).
//
// Configs whose clusters have no proxy-url pass through unchanged,
// preserving the legacy overwrite-on-PATCH behaviour for simple
// single-cluster setups (e.g. a real KUBECONFIG file with a stable name).
//
// Idempotent: re-running on already-suffixed names is a no-op.
func RewriteForRegistrant(cfg *clientcmdapi.Config) *clientcmdapi.Config {
	if cfg == nil {
		return nil
	}

	clusterSuffix := make(map[string]string, len(cfg.Clusters))
	for name, cluster := range cfg.Clusters {
		clusterSuffix[name] = DerivePortSuffix(cluster)
	}

	clusterRename := make(map[string]string, len(cfg.Clusters))
	for name := range cfg.Clusters {
		clusterRename[name] = withSuffix(clusterSuffix[name], name)
	}

	// Walk contexts in sorted order so an auth-info shared across multiple
	// contexts gets a deterministic suffix (the one from the first context
	// referencing it).
	ctxNames := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		ctxNames = append(ctxNames, n)
	}
	sort.Strings(ctxNames)

	ctxRename := make(map[string]string, len(cfg.Contexts))
	authSuffixForRef := make(map[string]string)
	for _, name := range ctxNames {
		ctx := cfg.Contexts[name]
		if ctx == nil {
			ctxRename[name] = name
			continue
		}
		s := clusterSuffix[ctx.Cluster] // "" when ctx.Cluster is unknown
		ctxRename[name] = withSuffix(s, name)
		if ctx.AuthInfo != "" {
			if _, seen := authSuffixForRef[ctx.AuthInfo]; !seen {
				authSuffixForRef[ctx.AuthInfo] = s
			}
		}
	}

	authRename := make(map[string]string, len(cfg.AuthInfos))
	for name := range cfg.AuthInfos {
		// Auth-infos not referenced by any context get no suffix — we can't
		// tell which cluster they "belong" to and renaming would break any
		// future context that wants to use them as-submitted.
		authRename[name] = withSuffix(authSuffixForRef[name], name)
	}

	out := clientcmdapi.NewConfig()
	out.APIVersion = cfg.APIVersion
	out.Kind = cfg.Kind
	out.Preferences = cfg.Preferences
	for k, v := range cfg.Extensions {
		out.Extensions[k] = v
	}

	for old, cluster := range cfg.Clusters {
		cp := *cluster
		out.Clusters[clusterRename[old]] = &cp
	}
	for old, ctx := range cfg.Contexts {
		if ctx == nil {
			continue
		}
		cp := *ctx
		if newCluster, ok := clusterRename[ctx.Cluster]; ok {
			cp.Cluster = newCluster
		}
		if newAuth, ok := authRename[ctx.AuthInfo]; ok {
			cp.AuthInfo = newAuth
		}
		out.Contexts[ctxRename[old]] = &cp
	}
	for old, auth := range cfg.AuthInfos {
		cp := *auth
		out.AuthInfos[authRename[old]] = &cp
	}
	if cfg.CurrentContext != "" {
		if newName, ok := ctxRename[cfg.CurrentContext]; ok {
			out.CurrentContext = newName
		} else {
			out.CurrentContext = cfg.CurrentContext
		}
	}

	return out
}
