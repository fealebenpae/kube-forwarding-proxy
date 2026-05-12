package k8s

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// injectEntryForContext is a context-aware variant of injectEntry: it plants a
// real TCP listener + vipState into the manager with key
// `<contextName>/<vipAddr>:8080`, matching the format produced by
// StartForward.
func injectEntryForContext(t *testing.T, m *ForwardManager, contextName, vipAddr string) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	key := contextName + "/" + vipAddr + ":8080"
	_, cancel := context.WithCancel(context.Background())
	entry := &forwardEntry{listener: ln, cancel: cancel}

	m.mu.Lock()
	m.entries[key] = entry
	if _, ok := m.vipStates[vipAddr]; !ok {
		m.vipStates[vipAddr] = &vipState{forwardKeys: []string{key}}
	} else {
		m.vipStates[vipAddr].forwardKeys = append(m.vipStates[vipAddr].forwardKeys, key)
	}
	m.mu.Unlock()

	return ln, key
}

// TestShutdownForContexts_FlushesNamedAndKeepsOthers is the core property:
// after a per-context shutdown the named context's listener is gone while
// every other context's listener keeps serving.
func TestShutdownForContexts_FlushesNamedAndKeepsOthers(t *testing.T) {
	m := NewForwardManager(nil, nil, zap.NewNop().Sugar(), nil, 0)

	keptLn, keptKey := injectEntryForContext(t, m, "ctx-keeper", "127.50.0.10")
	flushedLn, flushedKey := injectEntryForContext(t, m, "ctx-flush", "127.50.0.11")

	m.ShutdownForContexts([]string{"ctx-flush"})

	m.mu.Lock()
	_, keptStillThere := m.entries[keptKey]
	_, flushedStillThere := m.entries[flushedKey]
	m.mu.Unlock()

	if !keptStillThere {
		t.Errorf("listener for non-flushed context was incorrectly removed")
	}
	if flushedStillThere {
		t.Errorf("listener for flushed context was not removed")
	}

	if !listenerStillAccepting(keptLn) {
		t.Errorf("kept listener should still be accepting after per-context shutdown")
	}
	if listenerStillAccepting(flushedLn) {
		t.Errorf("flushed listener should be closed")
	}
}

// TestShutdownForContexts_ReleasesVIPOnlyWhenLastListenerGone exercises the
// VIP lifecycle: a VIP backing listeners from multiple contexts must stay
// allocated until every listener bound to it is torn down.
func TestShutdownForContexts_ReleasesVIPOnlyWhenLastListenerGone(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := NewForwardManager(nil, nil, zap.NewNop().Sugar(), releaser, 0)

	const sharedVIP = "127.50.0.20"
	_, keepKey := injectEntryForContext(t, m, "ctx-keeper", sharedVIP)
	_, flushKey := injectEntryForContext(t, m, "ctx-flush", sharedVIP)

	m.ShutdownForContexts([]string{"ctx-flush"})

	if got := len(releaser.releasedIPs()); got != 0 {
		t.Errorf("VIP released prematurely while ctx-keeper still has a listener; got %d release calls", got)
	}

	m.mu.Lock()
	state, ok := m.vipStates[sharedVIP]
	m.mu.Unlock()
	if !ok {
		t.Fatalf("vipState for %s was deleted while ctx-keeper's listener still active", sharedVIP)
	}
	if len(state.forwardKeys) != 1 || state.forwardKeys[0] != keepKey {
		t.Errorf("vipState.forwardKeys not pruned correctly; want [%s], got %v", keepKey, state.forwardKeys)
	}

	// Now shut down ctx-keeper too — VIP should be released.
	m.ShutdownForContexts([]string{"ctx-keeper"})
	if got := len(releaser.releasedIPs()); got != 1 {
		t.Errorf("expected 1 VIP release after last listener torn down; got %d", got)
	}
	m.mu.Lock()
	_, stillThere := m.vipStates[sharedVIP]
	m.mu.Unlock()
	if stillThere {
		t.Errorf("vipState should be removed after last listener torn down")
	}
	_ = flushKey
}

// TestShutdownForContexts_EmptyOrNilIsNoOp is a regression guard: callers
// (the kubeconfig handler) routinely produce an empty diff and shouldn't
// blow away anything in that case.
func TestShutdownForContexts_EmptyOrNilIsNoOp(t *testing.T) {
	m := NewForwardManager(nil, nil, zap.NewNop().Sugar(), nil, 0)
	_, key := injectEntryForContext(t, m, "ctx-keeper", "127.50.0.30")

	m.ShutdownForContexts(nil)
	m.ShutdownForContexts([]string{})

	m.mu.Lock()
	_, stillThere := m.entries[key]
	m.mu.Unlock()
	if !stillThere {
		t.Errorf("empty/nil names should not flush any listener")
	}
}

// TestShutdownForContexts_HonoursMultipleNames covers the multi-removal
// case (e.g. a PUT that replaces several contexts at once).
func TestShutdownForContexts_HonoursMultipleNames(t *testing.T) {
	m := NewForwardManager(nil, nil, zap.NewNop().Sugar(), nil, 0)
	_, k1 := injectEntryForContext(t, m, "ctx-1", "127.50.0.40")
	_, k2 := injectEntryForContext(t, m, "ctx-2", "127.50.0.41")
	_, k3 := injectEntryForContext(t, m, "ctx-3", "127.50.0.42")

	m.ShutdownForContexts([]string{"ctx-1", "ctx-3"})

	m.mu.Lock()
	_, e1 := m.entries[k1]
	_, e2 := m.entries[k2]
	_, e3 := m.entries[k3]
	m.mu.Unlock()
	if e1 || e3 {
		t.Errorf("ctx-1 / ctx-3 listeners should be gone; e1=%v e3=%v", e1, e3)
	}
	if !e2 {
		t.Errorf("ctx-2 listener should survive")
	}
}

// TestSplitEntryKey exercises the parser used by ShutdownForContexts.
func TestSplitEntryKey(t *testing.T) {
	cases := []struct {
		key  string
		ctx  string
		vip  string
		ok   bool
	}{
		{"ctx-a/127.50.0.1:8080", "ctx-a", "127.50.0.1", true},
		{"kind-kind-8030/127.50.0.5:27017", "kind-kind-8030", "127.50.0.5", true},
		// IPv6 VIPs aren't currently used but the parser should still
		// degrade gracefully on a non-conforming key — LastIndexByte
		// finds the last colon.
		{"ctx/[::1]:80", "ctx", "[::1]", true},
		{"no-slash", "", "", false},
		{"ctx/no-port", "", "", false},
	}
	for _, c := range cases {
		gotCtx, gotVIP, gotOK := splitEntryKey(c.key)
		if gotOK != c.ok || gotCtx != c.ctx || gotVIP != c.vip {
			t.Errorf("splitEntryKey(%q) = (%q,%q,%v); want (%q,%q,%v)",
				c.key, gotCtx, gotVIP, gotOK, c.ctx, c.vip, c.ok)
		}
	}
}

// listenerStillAccepting returns true when the listener is still bound to
// its port (a fresh Dial succeeds). Used to verify per-context shutdown
// keeps surviving listeners alive.
func listenerStillAccepting(ln net.Listener) bool {
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
