package k8s

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// minimalKubeconfig returns a syntactically valid kubeconfig YAML pointing to
// an unreachable server. kubernetes.NewForConfig succeeds without dialling, so
// these configs can be used in tests without a live cluster.
func minimalKubeconfig(clusterName, contextName, userName, server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %s
  cluster:
    server: %s
    insecure-skip-tls-verify: true
contexts:
- name: %s
  context:
    cluster: %s
    user: %s
current-context: %s
users:
- name: %s
  user:
    token: test-token
`, clusterName, server, contextName, clusterName, userName, contextName, userName)
}

// newTestHandler creates a KubeconfigHandler wired to a real ClientManager (no
// file path so it starts empty) and a nil ForwardManager.
func newTestHandler(t *testing.T) *KubeconfigHandler {
	t.Helper()
	cm, err := NewClientManager(zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("NewClientManager: %v", err)
	}
	h := &KubeconfigHandler{}
	h.SetManagers(cm, nil)
	return h
}

// do sends a request directly to the /kubeconfig handler via httptest.
func do(t *testing.T, h *KubeconfigHandler, method, body string) *http.Response {
	t.Helper()
	var rb io.Reader
	if body != "" {
		rb = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/kubeconfig", rb)
	rr := httptest.NewRecorder()
	h.handleKubeconfig(rr, req)
	return rr.Result()
}

func bodyStr(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return strings.TrimSpace(string(b))
}

// --- GET ---

func TestKubeconfig_GET_EmptyManager_Returns200(t *testing.T) {
	h := newTestHandler(t)
	resp := do(t, h, http.MethodGet, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if body := bodyStr(t, resp); !strings.Contains(body, "kind: Config") {
		t.Errorf("body does not look like a kubeconfig: %s", body)
	}
}

// --- PUT ---

func TestKubeconfig_PUT_ValidConfig_Returns204(t *testing.T) {
	h := newTestHandler(t)
	kc := minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")
	if resp := do(t, h, http.MethodPut, kc); resp.StatusCode != http.StatusNoContent {
		t.Errorf("PUT valid: status = %d, want 204; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_PUT_InvalidConfig_Returns400(t *testing.T) {
	h := newTestHandler(t)
	if resp := do(t, h, http.MethodPut, "this is not yaml at all: [[["); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT invalid: status = %d, want 400", resp.StatusCode)
	}
}

func TestKubeconfig_PUT_EmptyBody_Returns400(t *testing.T) {
	h := newTestHandler(t)
	if resp := do(t, h, http.MethodPut, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT empty: status = %d, want 400", resp.StatusCode)
	}
}

func TestKubeconfig_PUT_ReplacesExistingConfig(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("first PUT failed: %d", r.StatusCode)
	}
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c2", "ctx2", "user2", "https://localhost:9992")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("second PUT failed: %d", r.StatusCode)
	}
	body := bodyStr(t, do(t, h, http.MethodGet, ""))
	if strings.Contains(body, "ctx1") {
		t.Error("after second PUT, GET still contains ctx1")
	}
	if !strings.Contains(body, "ctx2") {
		t.Error("after second PUT, GET missing ctx2")
	}
}

// --- POST ---

func TestKubeconfig_POST_ValidNewConfig_Returns204(t *testing.T) {
	h := newTestHandler(t)
	kc := minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")
	if resp := do(t, h, http.MethodPost, kc); resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST valid: status = %d, want 204; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_POST_InvalidConfig_Returns400(t *testing.T) {
	h := newTestHandler(t)
	if resp := do(t, h, http.MethodPost, "not a kubeconfig"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST invalid: status = %d, want 400", resp.StatusCode)
	}
}

func TestKubeconfig_POST_DuplicateCluster_Returns409(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	if resp := do(t, h, http.MethodPost, minimalKubeconfig("c1", "ctx-new", "user-new", "https://localhost:9992")); resp.StatusCode != http.StatusConflict {
		t.Errorf("POST dup cluster: status = %d, want 409; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_POST_DuplicateContext_Returns409(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	if resp := do(t, h, http.MethodPost, minimalKubeconfig("c-new", "ctx1", "user-new", "https://localhost:9992")); resp.StatusCode != http.StatusConflict {
		t.Errorf("POST dup context: status = %d, want 409; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_POST_DuplicateUser_Returns409(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	if resp := do(t, h, http.MethodPost, minimalKubeconfig("c-new", "ctx-new", "user1", "https://localhost:9992")); resp.StatusCode != http.StatusConflict {
		t.Errorf("POST dup user: status = %d, want 409; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_POST_AppendsNewClusters(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	if r := do(t, h, http.MethodPost, minimalKubeconfig("c2", "ctx2", "user2", "https://localhost:9992")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("POST append failed: %d %s", r.StatusCode, bodyStr(t, r))
	}
	body := bodyStr(t, do(t, h, http.MethodGet, ""))
	for _, want := range []string{"ctx1", "ctx2", "c1", "c2"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET after POST: body missing %q\n%s", want, body)
		}
	}
}

// --- PATCH ---

func TestKubeconfig_PATCH_ValidConfig_Returns204(t *testing.T) {
	h := newTestHandler(t)
	kc := minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")
	if resp := do(t, h, http.MethodPatch, kc); resp.StatusCode != http.StatusNoContent {
		t.Errorf("PATCH valid: status = %d, want 204; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

func TestKubeconfig_PATCH_InvalidConfig_Returns400(t *testing.T) {
	h := newTestHandler(t)
	if resp := do(t, h, http.MethodPatch, "not yaml [["); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH invalid: status = %d, want 400", resp.StatusCode)
	}
}

func TestKubeconfig_PATCH_OverwritesDuplicates(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	// PATCH same cluster/context/user but with a different server.
	if r := do(t, h, http.MethodPatch, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9999")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH overwrite failed: %d %s", r.StatusCode, bodyStr(t, r))
	}
	if body := bodyStr(t, do(t, h, http.MethodGet, "")); !strings.Contains(body, "9999") {
		t.Errorf("after PATCH, server not updated in merged config:\n%s", body)
	}
}

// PATCH must NOT fail when cluster/context names already exist (unlike POST).
func TestKubeconfig_PATCH_DoesNotConflict(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	// Same names as seed — should succeed where POST would return 409.
	if resp := do(t, h, http.MethodPatch, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9998")); resp.StatusCode != http.StatusNoContent {
		t.Errorf("PATCH with duplicate names: status = %d, want 204; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

// --- DELETE ---

func TestKubeconfig_DELETE_Returns204(t *testing.T) {
	h := newTestHandler(t)
	if resp := do(t, h, http.MethodDelete, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE: status = %d, want 204", resp.StatusCode)
	}
}

func TestKubeconfig_DELETE_ClearsConfig(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT failed: %d", r.StatusCode)
	}
	if r := do(t, h, http.MethodDelete, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE failed: %d", r.StatusCode)
	}
	if body := bodyStr(t, do(t, h, http.MethodGet, "")); strings.Contains(body, "ctx1") {
		t.Errorf("after DELETE, GET still shows ctx1:\n%s", body)
	}
}

func TestKubeconfig_DELETE_AllowsRePut(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT failed: %d", r.StatusCode)
	}
	if r := do(t, h, http.MethodDelete, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE failed: %d", r.StatusCode)
	}
	// After DELETE, the same names should be POSTable again (no conflict).
	if resp := do(t, h, http.MethodPost, minimalKubeconfig("c1", "ctx1", "user1", "https://localhost:9991")); resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST after DELETE: status = %d, want 204; body: %s", resp.StatusCode, bodyStr(t, resp))
	}
}

// --- Method not allowed ---

func TestKubeconfig_UnsupportedMethod_Returns405(t *testing.T) {
	h := newTestHandler(t)
	for _, method := range []string{http.MethodHead, http.MethodOptions, "CUSTOM"} {
		resp := do(t, h, method, "")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("method %s: status = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "PUT") {
			t.Errorf("method %s: Allow header %q does not list PUT", method, allow)
		}
	}
}

// --- Nil manager ---

func TestKubeconfig_NilManager_Returns501(t *testing.T) {
	h := &KubeconfigHandler{} // no managers set
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if resp := do(t, h, method, ""); resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("method %s with nil manager: status = %d, want 501", method, resp.StatusCode)
		}
	}
}

// --- Round-trip PUT → GET ---

func TestKubeconfig_RoundTrip_PutThenGet(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("rt-cluster", "rt-ctx", "rt-user", "https://localhost:9997")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT failed: %d", r.StatusCode)
	}
	body := bodyStr(t, do(t, h, http.MethodGet, ""))
	for _, want := range []string{"rt-cluster", "rt-ctx", "rt-user"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET after PUT: body missing %q\n%s", want, body)
		}
	}
}

// --- POST conflict body mentions conflicting names ---

func TestKubeconfig_POST_ConflictBody_MentionsConflictingName(t *testing.T) {
	h := newTestHandler(t)
	if r := do(t, h, http.MethodPut, minimalKubeconfig("shared", "ctx1", "user1", "https://localhost:9991")); r.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT seed failed: %d", r.StatusCode)
	}
	resp := do(t, h, http.MethodPost, minimalKubeconfig("shared", "ctx-new", "user-new", "https://localhost:9992"))
	body := bodyStr(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "shared") {
		t.Errorf("conflict body %q does not mention conflicting name", body)
	}
}
