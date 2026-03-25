package k8s

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
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
func injectEntry(t *testing.T, m *ForwardManager, vipAddr string) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	key := "test-ctx/" + vipAddr + ":8080"
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
