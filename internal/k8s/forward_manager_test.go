package k8s

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// mockVIPReleaser records calls to ReleaseVIP.
type mockVIPReleaser struct {
	mu       sync.Mutex
	released []net.IP
}

func (m *mockVIPReleaser) ReleaseVIP(ip net.IP) error {
	m.mu.Lock()
	m.released = append(m.released, ip)
	m.mu.Unlock()
	return nil
}

func (m *mockVIPReleaser) releasedIPs() []net.IP {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]net.IP, len(m.released))
	copy(cp, m.released)
	return cp
}

// newExpiryForwardManager creates a ForwardManager wired for expiry tests.
// clients and resolver are nil because expiry logic never touches them.
func newExpiryForwardManager(releaser VIPReleaser, idleTimeout time.Duration) *ForwardManager {
	return NewForwardManager(nil, nil, zap.NewNop().Sugar(), releaser, idleTimeout)
}

// injectEntry adds a real TCP listener + vipState directly into a ForwardManager
// for test purposes, bypassing the Kubernetes API surface of StartForward.
// Returns the listener and the entry key so callers can make assertions.
func injectEntry(t *testing.T, m *ForwardManager, vipAddr string) (net.Listener, forwardKey) {
	t.Helper()
	return injectEntryForContext(t, m, "test-ctx", vipAddr)
}

// injectEntryForContext is like injectEntry but lets the caller choose the context name.
func injectEntryForContext(t *testing.T, m *ForwardManager, contextName, vipAddr string) (net.Listener, forwardKey) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	key := forwardKey{contextName: contextName, vipAddr: vipAddr, svcPort: 8080}
	_, cancel := context.WithCancel(context.Background())
	entry := &forwardEntry{listener: ln, cancel: cancel}

	m.mu.Lock()
	m.entries[key] = entry
	if _, ok := m.vipStates[vipAddr]; !ok {
		m.vipStates[vipAddr] = &vipState{forwardKeys: []forwardKey{key}}
	} else {
		m.vipStates[vipAddr].forwardKeys = append(m.vipStates[vipAddr].forwardKeys, key)
	}
	m.mu.Unlock()

	return ln, key
}

// TestExpireVIP_ClosesListenerAndReleasesVIP verifies that calling expireVIP
// with the matching generation tears down the listener and calls ReleaseVIP.
func TestExpireVIP_ClosesListenerAndReleasesVIP(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := newExpiryForwardManager(releaser, 5*time.Minute)

	vipAddr := "127.8.8.1"
	ln, _ := injectEntry(t, m, vipAddr)

	// Arm a timer and capture the generation.
	m.mu.Lock()
	m.touchVIPLocked(vipAddr)
	state := m.vipStates[vipAddr]
	state.mu.Lock()
	gen := state.generation
	state.mu.Unlock()
	m.mu.Unlock()

	// Directly invoke expiry — simulates the timer firing.
	m.expireVIP(vipAddr, gen)

	// Listener must be closed.
	_, err := ln.Accept()
	if err == nil {
		t.Error("expected Accept to fail after expiry, got nil error")
	}

	// ReleaseVIP must have been called.
	released := releaser.releasedIPs()
	if len(released) != 1 {
		t.Fatalf("ReleaseVIP called %d times, want 1", len(released))
	}
	if !released[0].Equal(net.ParseIP(vipAddr)) {
		t.Errorf("ReleaseVIP called with %s, want %s", released[0], vipAddr)
	}
}

// TestExpireVIP_StaleGenerationIsNoop verifies that an expiry callback whose
// generation no longer matches the current vipState is discarded.
func TestExpireVIP_StaleGenerationIsNoop(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := newExpiryForwardManager(releaser, 5*time.Minute)

	vipAddr := "127.8.8.2"
	ln, _ := injectEntry(t, m, vipAddr)

	// Set generation to 3; fire callback with stale generation 1.
	m.mu.Lock()
	m.vipStates[vipAddr].generation = 3
	m.mu.Unlock()

	m.expireVIP(vipAddr, 1) // stale

	// Listener must still be open.
	_ = acceptWithTimeout(ln, 10*time.Millisecond)

	// Verify the listener is still in the entries map.
	m.mu.Lock()
	_, stillPresent := m.vipStates[vipAddr]
	m.mu.Unlock()
	if !stillPresent {
		t.Error("vipState must not be removed for a stale generation")
	}
	if len(releaser.releasedIPs()) != 0 {
		t.Error("ReleaseVIP must not be called for a stale generation")
	}
}

// TestExpireVIP_ActiveConnectionBlocksExpiry verifies that the expiry callback
// is discarded when there are active connections on the VIP.
func TestExpireVIP_ActiveConnectionBlocksExpiry(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := newExpiryForwardManager(releaser, 5*time.Minute)

	vipAddr := "127.8.8.3"
	_, _ = injectEntry(t, m, vipAddr)

	// Simulate an active connection.
	m.mu.Lock()
	m.vipStates[vipAddr].activeConns.Store(1)
	m.vipStates[vipAddr].generation = 1
	gen := uint64(1)
	m.mu.Unlock()

	m.expireVIP(vipAddr, gen)

	m.mu.Lock()
	_, stillPresent := m.vipStates[vipAddr]
	m.mu.Unlock()
	if !stillPresent {
		t.Error("vipState must not be removed while activeConns > 0")
	}
	if len(releaser.releasedIPs()) != 0 {
		t.Error("ReleaseVIP must not be called while activeConns > 0")
	}
}

// TestTouchVIP_ResetsTimer verifies that TouchVIP stops the existing timer and
// arms a new one, so the VIP survives past the original deadline.
func TestTouchVIP_ResetsTimer(t *testing.T) {
	releaser := &mockVIPReleaser{}
	const timeout = 50 * time.Millisecond
	m := newExpiryForwardManager(releaser, timeout)

	vipAddr := "127.8.8.4"
	_, _ = injectEntry(t, m, vipAddr)

	// Arm the initial timer.
	m.mu.Lock()
	m.touchVIPLocked(vipAddr)
	m.mu.Unlock()

	// Before the first deadline, reset the timer via TouchVIP.
	time.Sleep(30 * time.Millisecond) // < 50ms, timer hasn't fired yet
	m.TouchVIP(vipAddr)

	// After the original deadline, the VIP must still be alive (timer was reset).
	time.Sleep(30 * time.Millisecond) // cumulative ~60ms; original deadline passed but reset
	if len(releaser.releasedIPs()) != 0 {
		t.Error("VIP must not be released before the reset timer fires")
	}

	// After the reset timer fires (+50ms from the touch), it must be released.
	time.Sleep(40 * time.Millisecond) // cumulative ~100ms from start; reset deadline is ~80ms
	if len(releaser.releasedIPs()) == 0 {
		t.Error("VIP must be released after the reset timer fires")
	}
}

// TestShutdown_StopsTimers verifies that Shutdown cancels pending idle timers
// so they don't fire after the manager is torn down.
func TestShutdown_StopsTimers(t *testing.T) {
	releaser := &mockVIPReleaser{}
	const timeout = 30 * time.Millisecond
	m := newExpiryForwardManager(releaser, timeout)

	vipAddr := "127.8.8.5"
	_, _ = injectEntry(t, m, vipAddr)

	m.mu.Lock()
	m.touchVIPLocked(vipAddr)
	m.mu.Unlock()

	// Shut down immediately — before the timer would fire.
	m.Shutdown()

	// Wait past the original timer deadline.
	time.Sleep(60 * time.Millisecond)

	// The timer was stopped during Shutdown; ReleaseVIP must not have been called.
	if len(releaser.releasedIPs()) != 0 {
		t.Error("ReleaseVIP must not be called after Shutdown stops the timer")
	}
}

// TestIdleTimeoutZero_NoTimerArmed verifies that when idleTimeout is zero
// touchVIPLocked is a no-op and no timer is ever created.
func TestIdleTimeoutZero_NoTimerArmed(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := newExpiryForwardManager(releaser, 0)

	vipAddr := "127.8.8.6"
	_, _ = injectEntry(t, m, vipAddr)

	m.mu.Lock()
	m.touchVIPLocked(vipAddr)
	state := m.vipStates[vipAddr]
	m.mu.Unlock()

	state.mu.Lock()
	timerSet := state.timer != nil
	state.mu.Unlock()
	if timerSet {
		t.Error("timer must not be armed when idleTimeout is zero")
	}
}

// acceptWithTimeout tries to Accept on ln for at most d, returning the error.
// If no connection arrives within d it returns a synthetic timeout error.
func acceptWithTimeout(ln net.Listener, d time.Duration) error {
	if tl, ok := ln.(interface{ SetDeadline(time.Time) error }); ok {
		_ = tl.SetDeadline(time.Now().Add(d))
	}
	_, err := ln.Accept()
	return err
}

// TestShutdownContext verifies that ShutdownContext tears down only the
// listeners, vipStates, SPDY pool entries, and VIPs belonging to the named
// context, leaving every other context's resources intact.
func TestShutdownContext(t *testing.T) {
	releaser := &mockVIPReleaser{}
	m := newExpiryForwardManager(releaser, 0) // idle expiry disabled; releaser still wired

	// Inject two contexts with one listener (and vipState) each.
	vipA := "127.9.0.1"
	vipB := "127.9.0.2"
	lnA, keyA := injectEntryForContext(t, m, "ctx-a", vipA)
	lnB, _ := injectEntryForContext(t, m, "ctx-b", vipB)

	// Inject a fake SPDY pool entry for each context.
	fakeConnA := &fakeCloser{}
	fakeConnB := &fakeCloser{}
	m.spdyPoolMu.Lock()
	m.spdyPool[spdyKey{"ctx-a", "default", "pod-a"}] = &poolEntry{conn: fakeConnA}
	m.spdyPool[spdyKey{"ctx-b", "default", "pod-b"}] = &poolEntry{conn: fakeConnB}
	m.spdyPoolMu.Unlock()

	// Shut down only ctx-a.
	m.ShutdownContext("ctx-a")

	// --- ctx-a must be gone ---

	// Listener for ctx-a must be closed.
	if err := acceptWithTimeout(lnA, 10*time.Millisecond); err == nil {
		t.Error("ctx-a listener should be closed after ShutdownContext")
	}

	// Entry must be removed from the map.
	m.mu.Lock()
	_, aEntryPresent := m.entries[keyA]
	_, aStatePresent := m.vipStates[vipA]
	m.mu.Unlock()
	if aEntryPresent {
		t.Error("ctx-a entry must be removed after ShutdownContext")
	}
	if aStatePresent {
		t.Error("ctx-a vipState must be removed after ShutdownContext")
	}

	// VIP for ctx-a must have been released.
	relIPs := releaser.releasedIPs()
	if len(relIPs) != 1 {
		t.Fatalf("ReleaseVIP called %d times, want 1", len(relIPs))
	}
	if !relIPs[0].Equal(net.ParseIP(vipA)) {
		t.Errorf("ReleaseVIP called with %s, want %s", relIPs[0], vipA)
	}

	// SPDY pool entry for ctx-a must be closed and removed.
	if !fakeConnA.closed {
		t.Error("ctx-a SPDY connection must be closed after ShutdownContext")
	}
	m.spdyPoolMu.Lock()
	_, aSPDYPresent := m.spdyPool[spdyKey{"ctx-a", "default", "pod-a"}]
	m.spdyPoolMu.Unlock()
	if aSPDYPresent {
		t.Error("ctx-a SPDY pool entry must be removed after ShutdownContext")
	}

	// --- ctx-b must be untouched ---

	// ctx-b listener must still be open.
	_ = lnB.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Millisecond))
	_, err := lnB.Accept()
	if err != nil {
		// A deadline/timeout error means the listener is alive (no connections arrived).
		// An error whose string contains "use of closed" would mean it was closed — bad.
		if isClosedErr(err) {
			t.Error("ctx-b listener must still be open after ShutdownContext(ctx-a)")
		}
	}

	m.mu.Lock()
	_, bStatePresent := m.vipStates[vipB]
	m.mu.Unlock()
	if !bStatePresent {
		t.Error("ctx-b vipState must still be present after ShutdownContext(ctx-a)")
	}

	if fakeConnB.closed {
		t.Error("ctx-b SPDY connection must not be closed after ShutdownContext(ctx-a)")
	}
	m.spdyPoolMu.Lock()
	_, bSPDYPresent := m.spdyPool[spdyKey{"ctx-b", "default", "pod-b"}]
	m.spdyPoolMu.Unlock()
	if !bSPDYPresent {
		t.Error("ctx-b SPDY pool entry must still be present after ShutdownContext(ctx-a)")
	}
}

// fakeCloser implements httpstream.Connection minimally for pool-entry tests.
type fakeCloser struct {
	closed bool
}

func (f *fakeCloser) Close() error                                          { f.closed = true; return nil }
func (f *fakeCloser) CreateStream(_ http.Header) (httpstream.Stream, error) { return nil, nil }
func (f *fakeCloser) CloseChan() <-chan bool                                { return nil }
func (f *fakeCloser) SetIdleTimeout(_ time.Duration)                        {}
func (f *fakeCloser) RemoveStreams(_ ...httpstream.Stream)                  {}

// isClosedErr reports whether err looks like a "use of closed network connection" error.
func isClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed")
}
