// Package health provides HTTP handlers for liveness and readiness probes.
package health

import (
	"net/http"
	"sync/atomic"
)

// Handler holds the readiness state for the /healthz and /readyz probes.
type Handler struct {
	ready atomic.Bool
}

// AddToMux registers /healthz and /readyz on mux and returns the Handler
// whose SetReady method should be called once the proxy is fully initialised.
func AddToMux(mux *http.ServeMux) *Handler {
	h := &Handler{}
	mux.HandleFunc("/healthz", h.handleLiveness)
	mux.HandleFunc("/readyz", h.handleReadiness)
	return h
}

// SetReady marks the proxy as ready to serve traffic.
func (h *Handler) SetReady() {
	h.ready.Store(true)
}

func (h *Handler) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if h.ready.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	}
}
