package k8s

import (
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestDerivePortSuffix(t *testing.T) {
	cases := []struct {
		name    string
		cluster *clientcmdapi.Cluster
		want    string
	}{
		{"nil cluster", nil, ""},
		{"no proxy-url", &clientcmdapi.Cluster{Server: "https://x"}, ""},
		{"empty proxy-url", &clientcmdapi.Cluster{ProxyURL: ""}, ""},
		{"http with port", &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}, "8026"},
		{"https with port", &clientcmdapi.Cluster{ProxyURL: "https://proxy:8443"}, "8443"},
		{"socks5 with port", &clientcmdapi.Cluster{ProxyURL: "socks5://127.0.0.1:1081"}, "1081"},
		{"http no port → 80", &clientcmdapi.Cluster{ProxyURL: "http://proxy"}, "80"},
		{"https no port → 443", &clientcmdapi.Cluster{ProxyURL: "https://proxy"}, "443"},
		{"socks5 no port → 1080", &clientcmdapi.Cluster{ProxyURL: "socks5://127.0.0.1"}, "1080"},
		{"socks5h no port → 1080", &clientcmdapi.Cluster{ProxyURL: "socks5h://127.0.0.1"}, "1080"},
		{"unknown scheme no port", &clientcmdapi.Cluster{ProxyURL: "weird://host"}, ""},
		{"malformed url", &clientcmdapi.Cluster{ProxyURL: "://broken"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePortSuffix(tc.cluster); got != tc.want {
				t.Errorf("DerivePortSuffix(%+v) = %q, want %q", tc.cluster, got, tc.want)
			}
		})
	}
}

func TestRewriteForRegistrant_SuffixesAndFixesCrossRefs(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["kind-kind"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}
	cfg.AuthInfos["kind-kind"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["kind-kind"] = &clientcmdapi.Context{Cluster: "kind-kind", AuthInfo: "kind-kind", Namespace: "default"}
	cfg.CurrentContext = "kind-kind"

	out := RewriteForRegistrant(cfg)

	if _, ok := out.Clusters["kind-kind-8026"]; !ok {
		t.Errorf("missing renamed cluster kind-kind-8026 in: %v", keysOf(out.Clusters))
	}
	if _, ok := out.Contexts["kind-kind-8026"]; !ok {
		t.Errorf("missing renamed context kind-kind-8026 in: %v", keysOf(out.Contexts))
	}
	if _, ok := out.AuthInfos["kind-kind-8026"]; !ok {
		t.Errorf("missing renamed auth-info kind-kind-8026 in: %v", keysOf(out.AuthInfos))
	}
	ctx := out.Contexts["kind-kind-8026"]
	if ctx.Cluster != "kind-kind-8026" {
		t.Errorf("ctx.Cluster = %q, want kind-kind-8026", ctx.Cluster)
	}
	if ctx.AuthInfo != "kind-kind-8026" {
		t.Errorf("ctx.AuthInfo = %q, want kind-kind-8026", ctx.AuthInfo)
	}
	if ctx.Namespace != "default" {
		t.Errorf("ctx.Namespace lost: %q", ctx.Namespace)
	}
	if out.CurrentContext != "kind-kind-8026" {
		t.Errorf("CurrentContext = %q, want kind-kind-8026", out.CurrentContext)
	}
}

func TestRewriteForRegistrant_NoSuffixWhenNoProxyURL(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c1"] = &clientcmdapi.Cluster{Server: "https://x"}
	cfg.AuthInfos["u1"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["ctx1"] = &clientcmdapi.Context{Cluster: "c1", AuthInfo: "u1"}
	cfg.CurrentContext = "ctx1"

	out := RewriteForRegistrant(cfg)
	if _, ok := out.Clusters["c1"]; !ok {
		t.Errorf("expected cluster c1 unchanged, got: %v", keysOf(out.Clusters))
	}
	if _, ok := out.Contexts["ctx1"]; !ok {
		t.Errorf("expected context ctx1 unchanged, got: %v", keysOf(out.Contexts))
	}
	if _, ok := out.AuthInfos["u1"]; !ok {
		t.Errorf("expected auth-info u1 unchanged, got: %v", keysOf(out.AuthInfos))
	}
	if out.CurrentContext != "ctx1" {
		t.Errorf("CurrentContext = %q, want ctx1", out.CurrentContext)
	}
}

func TestRewriteForRegistrant_Idempotent(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["kind-kind"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}
	cfg.AuthInfos["kind-kind"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["kind-kind"] = &clientcmdapi.Context{Cluster: "kind-kind", AuthInfo: "kind-kind"}
	cfg.CurrentContext = "kind-kind"

	once := RewriteForRegistrant(cfg)
	twice := RewriteForRegistrant(once)
	if !sameKeys(once.Clusters, twice.Clusters) ||
		!sameKeys(once.Contexts, twice.Contexts) ||
		!sameKeys(once.AuthInfos, twice.AuthInfos) {
		t.Errorf("not idempotent: once=%v twice=%v",
			keysOf(once.Contexts), keysOf(twice.Contexts))
	}
	if once.CurrentContext != twice.CurrentContext {
		t.Errorf("CurrentContext drifted: %q -> %q", once.CurrentContext, twice.CurrentContext)
	}
	// Sanity: no double suffix.
	for name := range twice.Clusters {
		if name == "kind-kind-8026-8026" {
			t.Errorf("double suffix leaked: %v", keysOf(twice.Clusters))
		}
	}
}

func TestRewriteForRegistrant_DifferentPortsCoexist(t *testing.T) {
	// Two configs with the same kind context name but different proxy ports
	// — different worktrees, different tunnels — should produce distinct
	// suffixed names.
	a := clientcmdapi.NewConfig()
	a.Clusters["kind-kind"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}
	a.Contexts["kind-kind"] = &clientcmdapi.Context{Cluster: "kind-kind"}
	b := clientcmdapi.NewConfig()
	b.Clusters["kind-kind"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8027"}
	b.Contexts["kind-kind"] = &clientcmdapi.Context{Cluster: "kind-kind"}

	outA := RewriteForRegistrant(a)
	outB := RewriteForRegistrant(b)
	if _, ok := outA.Contexts["kind-kind-8026"]; !ok {
		t.Errorf("A missing kind-kind-8026: %v", keysOf(outA.Contexts))
	}
	if _, ok := outB.Contexts["kind-kind-8027"]; !ok {
		t.Errorf("B missing kind-kind-8027: %v", keysOf(outB.Contexts))
	}
}

func TestRewriteForRegistrant_SameNameSamePortCollapses(t *testing.T) {
	// Same context name, same proxy port — produces the same final name,
	// so a subsequent PATCH naturally overwrites the previous entry.
	a := clientcmdapi.NewConfig()
	a.Clusters["kind-kind"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}
	a.Contexts["kind-kind"] = &clientcmdapi.Context{Cluster: "kind-kind"}

	outA1 := RewriteForRegistrant(a)
	outA2 := RewriteForRegistrant(a)
	if !sameKeys(outA1.Contexts, outA2.Contexts) {
		t.Errorf("same input produced different shapes: %v vs %v",
			keysOf(outA1.Contexts), keysOf(outA2.Contexts))
	}
}

func TestRewriteForRegistrant_NilInput(t *testing.T) {
	if got := RewriteForRegistrant(nil); got != nil {
		t.Errorf("RewriteForRegistrant(nil) = %v, want nil", got)
	}
}

func TestRewriteForRegistrant_OrphanAuthInfoNotSuffixed(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["c1"] = &clientcmdapi.Cluster{ProxyURL: "http://127.0.0.1:8026"}
	cfg.AuthInfos["orphan"] = &clientcmdapi.AuthInfo{Token: "t"}
	cfg.Contexts["ctx1"] = &clientcmdapi.Context{Cluster: "c1"} // no AuthInfo

	out := RewriteForRegistrant(cfg)
	if _, ok := out.AuthInfos["orphan"]; !ok {
		t.Errorf("orphan auth-info should pass through unchanged; got keys: %v", keysOf(out.AuthInfos))
	}
}

func TestRewriteForRegistrant_OrphanContextWithUnknownClusterNotSuffixed(t *testing.T) {
	cfg := clientcmdapi.NewConfig()
	cfg.Contexts["ctx1"] = &clientcmdapi.Context{Cluster: "missing"}

	out := RewriteForRegistrant(cfg)
	if _, ok := out.Contexts["ctx1"]; !ok {
		t.Errorf("context with unknown cluster should pass through; got keys: %v", keysOf(out.Contexts))
	}
}

// keysOf returns the keys of any string-keyed map as a sorted slice. Used by
// tests for stable assertion messages.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func sameKeys[V any](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}
