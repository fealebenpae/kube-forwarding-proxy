package k8s

import (
	"net"
	netUrl "net/url"
	"sort"
	"strings"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DerivePortSuffix returns the port from a cluster's proxy-url, or "" when
// no proxy-url is set or the URL is unparseable. When proxy-url has no
// explicit port the scheme's default is used: http→80, https→443,
// socks5/socks5h→1080.
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

// ContextNameMap returns submitted-name -> registered-name for every context
// in cfg, applying the same suffixing rules as RewriteForRegistrant. Used by
// the `register` subcommand to preview the rename without round-tripping the
// full rewrite output.
func ContextNameMap(cfg *clientcmdapi.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	out := make(map[string]string, len(cfg.Contexts))
	for name, ctx := range cfg.Contexts {
		registered := name
		if ctx != nil {
			registered = withSuffix(DerivePortSuffix(cfg.Clusters[ctx.Cluster]), name)
		}
		out[name] = registered
	}
	return out
}

// RewriteForRegistrant returns a copy of cfg with cluster, context, and
// auth-info names suffixed by their cluster's proxy-url port. Cross-references
// (and CurrentContext) are rewritten to the new names. Identity becomes
// (original-name, proxy-port): different ports stay independently
// addressable; same name+port PATCH-overwrites. Clusters without a
// proxy-url pass through unchanged. Idempotent.
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
