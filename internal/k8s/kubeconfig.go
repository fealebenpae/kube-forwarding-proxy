package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// EmptyConfig is a blank kubeconfig used as a safe zero value.
var EmptyConfig = clientcmdapi.NewConfig()

// KubeconfigHandler implements the /kubeconfig HTTP endpoint.
//
// It exposes:
//
//	GET    - returns the merged kubeconfig as YAML (200).
//	PUT    - replaces the dynamic config; 204 on success, 400 on invalid input.
//	POST   - appends clusters/contexts/users without overwriting; 204 on success,
//	         400 on invalid input, 409 on conflict.
//	PATCH  - merges with overwrite; 204 on success, 400 on invalid input.
//	DELETE - clears the dynamic config; 204.
//
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

// SetManagers injects the ClientManager and ForwardManager after the handler
// has been registered. This allows the HTTP server to start before the managers
// are fully initialised, while still serving the /kubeconfig endpoint once ready.
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
		prev := cm.MergedConfig()
		if err := cm.Add(RewriteForRegistrant(parsed)); err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		// Add() rejects on conflict, so surviving contexts can't have changed
		// — only new ones were appended. Nothing to flush.
		_ = prev
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

// shutdownTunnelsForContexts shuts down port-forward tunnels for the listed
// kubeconfig context names. Tunnels for contexts not in the list — i.e.
// peer worktrees that are sharing the daemon and whose cluster/auth data
// did not change — keep running and serving in-flight TCP connections. The
// previous always-on-shutdown semantics are equivalent to passing every
// context name in the merged config.
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

// ContextsToFlush returns the names of contexts whose port-forward listeners
// must be torn down when the merged kubeconfig changes from old to new.
// Contexts are flushed when they are removed entirely, or when their
// referenced cluster's apiserver endpoint, certificate-authority data, or
// proxy-url no longer matches; or when their referenced auth-info data
// (client cert / key / token) changes. Contexts whose definitions are
// byte-for-byte equivalent across old and new keep their listeners — this
// is what lets parallel worktrees share the daemon without disrupting each
// other's in-flight TCP connections on every peer registration.
//
// new == nil is treated as "everything was deleted" — every context in old
// is flushed. old == nil short-circuits to no-op (nothing was forwarding).
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

// clustersEquivalent compares the fields of two Cluster entries that
// determine whether existing port-forward listeners are still valid:
// apiserver Server URL, certificate-authority data, and proxy-url. Other
// fields (e.g. InsecureSkipTLSVerify, TLSServerName) also affect TLS but
// are not part of the day-to-day churn between worktrees re-registering
// the same cluster, so they're treated as significant here too.
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

// authInfosEquivalent compares the fields of two AuthInfo entries that
// determine client identity for an in-flight port-forward: client cert/key
// data, token, and impersonation. Username/password are also compared even
// though kfp doesn't use basic-auth, so a future migration doesn't silently
// keep stale listeners.
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
