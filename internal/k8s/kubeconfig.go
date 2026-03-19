package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
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
		if err := cm.Reset(parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.shutdownTunnels()
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
		if err := cm.Add(parsed); err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				http.Error(w, err.Error(), http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		h.shutdownTunnels()
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
		if err := cm.MergeAndOverwrite(parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.shutdownTunnels()
		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		_ = cm.Reset(nil)
		h.shutdownTunnels()
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, PUT, POST, PATCH, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// shutdownTunnels shuts down all active port-forward tunnels if a ForwardManager
// is configured. Tunnels will be re-established on next use.
func (h *KubeconfigHandler) shutdownTunnels() {
	h.mu.RLock()
	fm := h.forwardManager
	h.mu.RUnlock()
	if fm != nil {
		fm.Shutdown()
	}
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
