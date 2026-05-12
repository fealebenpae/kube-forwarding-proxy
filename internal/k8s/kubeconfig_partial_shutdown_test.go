package k8s

import (
	"reflect"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// TestContextsToFlush_NoOpOnIdenticalConfigs verifies the central property
// that makes multi-worktree sharing safe: re-applying a byte-for-byte
// equivalent kubeconfig produces no flush list.
func TestContextsToFlush_NoOpOnIdenticalConfigs(t *testing.T) {
	cfg := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", caData: []byte("ca-a")}, auth: authSpec{token: "t-a"}},
		"ctx-b": {cluster: clusterSpec{server: "https://b", caData: []byte("ca-b"), proxyURL: "socks5://127.0.0.1:1080"}, auth: authSpec{token: "t-b"}},
	})
	cfgClone := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", caData: []byte("ca-a")}, auth: authSpec{token: "t-a"}},
		"ctx-b": {cluster: clusterSpec{server: "https://b", caData: []byte("ca-b"), proxyURL: "socks5://127.0.0.1:1080"}, auth: authSpec{token: "t-b"}},
	})
	if got := ContextsToFlush(cfg, cfgClone); len(got) != 0 {
		t.Fatalf("expected no contexts to flush across identical configs; got %v", got)
	}
}

func TestContextsToFlush_RemovedContext(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
		"ctx-b": {cluster: clusterSpec{server: "https://b"}, auth: authSpec{token: "t-b"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
	})
	got := ContextsToFlush(old, new)
	if !reflect.DeepEqual(got, []string{"ctx-b"}) {
		t.Fatalf("expected [ctx-b], got %v", got)
	}
}

func TestContextsToFlush_ChangedServer(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a-rotated"}, auth: authSpec{token: "t-a"}},
	})
	got := ContextsToFlush(old, new)
	if !reflect.DeepEqual(got, []string{"ctx-a"}) {
		t.Fatalf("expected [ctx-a] for server change, got %v", got)
	}
}

func TestContextsToFlush_ChangedCAData(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", caData: []byte("old-ca")}, auth: authSpec{token: "t-a"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", caData: []byte("new-ca")}, auth: authSpec{token: "t-a"}},
	})
	got := ContextsToFlush(old, new)
	if !reflect.DeepEqual(got, []string{"ctx-a"}) {
		t.Fatalf("expected [ctx-a] for CA rotation, got %v", got)
	}
}

func TestContextsToFlush_ChangedProxyURL(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", proxyURL: "socks5://127.0.0.1:1080"}, auth: authSpec{token: "t-a"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a", proxyURL: "socks5://127.0.0.1:1081"}, auth: authSpec{token: "t-a"}},
	})
	got := ContextsToFlush(old, new)
	if !reflect.DeepEqual(got, []string{"ctx-a"}) {
		t.Fatalf("expected [ctx-a] for proxy-url change, got %v", got)
	}
}

func TestContextsToFlush_ChangedToken(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "old-token"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "new-token"}},
	})
	got := ContextsToFlush(old, new)
	if !reflect.DeepEqual(got, []string{"ctx-a"}) {
		t.Fatalf("expected [ctx-a] for token change, got %v", got)
	}
}

// TestContextsToFlush_AddingNewContextLeavesPeerAlone is the property that
// makes parallel-worktree sharing work: when a second worktree registers,
// the daemon must not flush the first worktree's listeners.
func TestContextsToFlush_AddingNewContextLeavesPeerAlone(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
	})
	new := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
		"ctx-b": {cluster: clusterSpec{server: "https://b"}, auth: authSpec{token: "t-b"}},
	})
	if got := ContextsToFlush(old, new); len(got) != 0 {
		t.Fatalf("expected no flush when peer context is added; got %v", got)
	}
}

func TestContextsToFlush_NilOldReturnsNothing(t *testing.T) {
	new := buildConfig(map[string]contextSpec{"x": {cluster: clusterSpec{server: "https://x"}, auth: authSpec{token: "t"}}})
	if got := ContextsToFlush(nil, new); len(got) != 0 {
		t.Fatalf("nil old should produce no flush; got %v", got)
	}
}

func TestContextsToFlush_NilNewFlushesEverything(t *testing.T) {
	old := buildConfig(map[string]contextSpec{
		"ctx-a": {cluster: clusterSpec{server: "https://a"}, auth: authSpec{token: "t-a"}},
		"ctx-b": {cluster: clusterSpec{server: "https://b"}, auth: authSpec{token: "t-b"}},
	})
	got := ContextsToFlush(old, nil)
	if !reflect.DeepEqual(got, []string{"ctx-a", "ctx-b"}) {
		t.Fatalf("expected all contexts flushed on nil new; got %v", got)
	}
}

type clusterSpec struct {
	server   string
	caData   []byte
	proxyURL string
}

type authSpec struct {
	token string
}

type contextSpec struct {
	cluster clusterSpec
	auth    authSpec
}

func buildConfig(contexts map[string]contextSpec) *clientcmdapi.Config {
	cfg := clientcmdapi.NewConfig()
	for name, spec := range contexts {
		clusterName := name + "-cluster"
		authName := name + "-auth"
		cfg.Clusters[clusterName] = &clientcmdapi.Cluster{
			Server:                   spec.cluster.server,
			CertificateAuthorityData: append([]byte(nil), spec.cluster.caData...),
			ProxyURL:                 spec.cluster.proxyURL,
		}
		cfg.AuthInfos[authName] = &clientcmdapi.AuthInfo{
			Token: spec.auth.token,
		}
		cfg.Contexts[name] = &clientcmdapi.Context{
			Cluster:  clusterName,
			AuthInfo: authName,
		}
	}
	return cfg
}
