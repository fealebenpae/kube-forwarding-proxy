package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"sync"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// EmptyConfig is a blank kubeconfig used as a safe zero value.
var EmptyConfig = clientcmdapi.NewConfig()

// KubeconfigHandler implements the /kubeconfig HTTP endpoint
// (GET / PUT / POST / PATCH / DELETE). See README for semantics.
// Call SetManagers to inject dependencies after the managers are initialised.
type KubeconfigHandler struct {
	mu             sync.RWMutex
	clientManager  *ClientManager
	forwardManager *ForwardManager
}

// AddToMux registers /kubeconfig on mux and returns the KubeconfigHandler
// whose SetManagers method should be called once the k8s managers are ready.
func AddToMux(mux *http.ServeMux) *KubeconfigHandler {
	h := &KubeconfigHandler{}
	mux.HandleFunc("/kubeconfig", h.handleKubeconfig)
	return h
}

// SetManagers injects the live managers after the handler is registered, so
// the HTTP server can start before k8s init completes.
func (h *KubeconfigHandler) SetManagers(cm *ClientManager, fm *ForwardManager) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clientManager = cm
	h.forwardManager = fm
}

func (h *KubeconfigHandler) handleKubeconfig(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	cm := h.clientManager
	h.mu.RUnlock()

	if cm == nil {
		http.Error(w, "kubeconfig management not configured", http.StatusNotImplemented)
		return
	}

	switch r.Method {
	case http.MethodGet:
		kubecfg := cm.Kubeconfig()
		if kubecfg == nil {
			kubecfg = EmptyConfig
		}
		data, err := clientcmd.Write(*kubecfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		parsed, err := parseAndValidate(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prev := cm.MergedConfig()
		if err := cm.Reset(RewriteForRegistrant(parsed)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.shutdownTunnelsForContexts(ContextsToFlush(prev, cm.MergedConfig()))
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		parsed, err := parseAndValidate(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := cm.Add(RewriteForRegistrant(parsed)); err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		// Add() rejects on conflict, so nothing to flush — surviving
		// contexts didn't change; only new ones were appended.
		w.WriteHeader(http.StatusNoContent)

	case http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		parsed, err := parseAndValidate(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		prev := cm.MergedConfig()
		if err := cm.MergeAndOverwrite(RewriteForRegistrant(parsed)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.shutdownTunnelsForContexts(ContextsToFlush(prev, cm.MergedConfig()))
		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		prev := cm.MergedConfig()
		_ = cm.Reset(nil)
		h.shutdownTunnelsForContexts(ContextsToFlush(prev, cm.MergedConfig()))
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, PUT, POST, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// shutdownTunnelsForContexts tears down tunnels only for the named contexts,
// so unrelated contexts whose cluster/auth data didn't change keep serving
// in-flight TCP connections.
func (h *KubeconfigHandler) shutdownTunnelsForContexts(names []string) {
	if len(names) == 0 {
		return
	}
	h.mu.RLock()
	fm := h.forwardManager
	h.mu.RUnlock()
	if fm != nil {
		fm.ShutdownForContexts(names)
	}
}

// ContextsToFlush returns the names of contexts whose listeners must be torn
// down when the merged kubeconfig changes from old to new — i.e. contexts
// that are gone, or whose referenced cluster/auth fields differ. Equivalent
// contexts keep their listeners. new == nil flushes every context in old;
// old == nil is a no-op.
func ContextsToFlush(old, new *clientcmdapi.Config) []string {
	if old == nil || len(old.Contexts) == 0 {
		return nil
	}
	flush := make([]string, 0)
	for name, oldCtx := range old.Contexts {
		if new == nil {
			flush = append(flush, name)
			continue
		}
		newCtx, ok := new.Contexts[name]
		if !ok {
			flush = append(flush, name)
			continue
		}
		// A context that now points at a different cluster/auth-info is a
		// different upstream even if both bodies are unchanged.
		if oldCtx.Cluster != newCtx.Cluster || oldCtx.AuthInfo != newCtx.AuthInfo {
			flush = append(flush, name)
			continue
		}
		if !clustersEquivalent(old.Clusters[oldCtx.Cluster], new.Clusters[newCtx.Cluster]) {
			flush = append(flush, name)
			continue
		}
		if !authInfosEquivalent(old.AuthInfos[oldCtx.AuthInfo], new.AuthInfos[newCtx.AuthInfo]) {
			flush = append(flush, name)
			continue
		}
	}
	sort.Strings(flush)
	return flush
}

// clustersEquivalent reports whether two Cluster entries match on every
// field that would invalidate an existing port-forward listener.
func clustersEquivalent(a, b *clientcmdapi.Cluster) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Server != b.Server || a.ProxyURL != b.ProxyURL {
		return false
	}
	if a.InsecureSkipTLSVerify != b.InsecureSkipTLSVerify || a.TLSServerName != b.TLSServerName {
		return false
	}
	if a.CertificateAuthority != b.CertificateAuthority {
		return false
	}
	if !bytes.Equal(a.CertificateAuthorityData, b.CertificateAuthorityData) {
		return false
	}
	return true
}

// authInfosEquivalent reports whether two AuthInfo entries share identical
// credentials (cert/key, token, impersonation, basic-auth, exec plugin, auth
// provider). Exec / auth-provider rotation must invalidate listeners — e.g.
// aws-iam-authenticator or gke-gcloud-auth-plugin reissuing on a different
// account.
func authInfosEquivalent(a, b *clientcmdapi.AuthInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Token != b.Token || a.TokenFile != b.TokenFile {
		return false
	}
	if a.ClientCertificate != b.ClientCertificate || a.ClientKey != b.ClientKey {
		return false
	}
	if !bytes.Equal(a.ClientCertificateData, b.ClientCertificateData) {
		return false
	}
	if !bytes.Equal(a.ClientKeyData, b.ClientKeyData) {
		return false
	}
	if a.Username != b.Username || a.Password != b.Password {
		return false
	}
	if a.Impersonate != b.Impersonate {
		return false
	}
	if !reflect.DeepEqual(a.Exec, b.Exec) {
		return false
	}
	if !reflect.DeepEqual(a.AuthProvider, b.AuthProvider) {
		return false
	}
	return true
}

// parseAndValidate decodes raw bytes as a kubeconfig and performs basic
// structural validation. Returns a descriptive error for malformed input.
func parseAndValidate(raw []byte) (*clientcmdapi.Config, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty kubeconfig")
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid kubeconfig: %w", err)
	}
	return cfg, nil
}
