package vip

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

type mockAddressManager struct {
	AddVIP func(ip net.IP) error
}

func (m *mockAddressManager) AddAddress(ip net.IP) error {
	if m.AddVIP != nil {
		return m.AddVIP(ip)
	}
	return nil
}

func (m *mockAddressManager) RemoveAddress(_ net.IP) error {
	return nil
}

func TestNewAllocator(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}
	if a == nil {
		t.Fatal("allocator is nil")
	}
}

func TestNewAllocator_InvalidCIDR(t *testing.T) {
	_, err := NewAllocator(&mockAddressManager{}, "not-a-cidr")
	if err == nil {
		t.Fatal("expected error for invalid CIDR, got nil")
	}
}

func TestAllocate_Basic(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	ip1, err := a.Allocate("default/svc-a")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	expected := net.IPv4(198, 18, 0, 1).To4()
	if !ip1.Equal(expected) {
		t.Errorf("got %s, want %s", ip1, expected)
	}

	ip2, err := a.Allocate("default/svc-b")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	expected2 := net.IPv4(198, 18, 0, 2).To4()
	if !ip2.Equal(expected2) {
		t.Errorf("got %s, want %s", ip2, expected2)
	}
}

func TestAllocate_Dedup(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	ip1, err := a.Allocate("default/svc-a")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	ip2, err := a.Allocate("default/svc-a")
	if err != nil {
		t.Fatalf("second Allocate failed: %v", err)
	}

	if !ip1.Equal(ip2) {
		t.Errorf("duplicate key returned different IPs: %s vs %s", ip1, ip2)
	}
}

func TestAllocate_CIDRExhaustion(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/30")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	_, err = a.Allocate("default/svc-1")
	if err != nil {
		t.Fatalf("Allocate 1 failed: %v", err)
	}

	_, err = a.Allocate("default/svc-2")
	if err != nil {
		t.Fatalf("Allocate 2 failed: %v", err)
	}

	_, err = a.Allocate("default/svc-3")
	if err == nil {
		t.Fatal("expected CIDR exhaustion error, got nil")
	}
}

func TestLookup(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	if ip := a.Lookup("default/svc-a"); ip != nil {
		t.Errorf("expected nil for unallocated service, got %s", ip)
	}

	allocated, _ := a.Allocate("default/svc-a")
	looked := a.Lookup("default/svc-a")
	if !looked.Equal(allocated) {
		t.Errorf("Lookup returned %s, want %s", looked, allocated)
	}
}

func TestReverseLookup(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	ip, _ := a.Allocate("kube-system/my-svc")
	key := a.ReverseLookup(ip.String())
	if key != "kube-system/my-svc" {
		t.Errorf("ReverseLookup returned %q, want %q", key, "kube-system/my-svc")
	}

	unknown := a.ReverseLookup("1.2.3.4")
	if unknown != "" {
		t.Errorf("ReverseLookup for unknown IP returned %q, want empty", unknown)
	}
}

func TestAllAllocations(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	_, _ = a.Allocate("default/svc-a")
	_, _ = a.Allocate("default/svc-b")

	allocs := a.AllAllocations()
	if len(allocs) != 2 {
		t.Errorf("AllAllocations returned %d entries, want 2", len(allocs))
	}
}

func TestAllocate_Concurrent(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/16")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	var wg sync.WaitGroup
	errors := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("default/svc-%d", i)
			_, err := a.Allocate(key)
			if err != nil {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Errorf("concurrent allocation error: %v", err)
	}
}

func TestAddVIP_Hook(t *testing.T) {
	var addedIPs []string
	addressManager := &mockAddressManager{
		AddVIP: func(ip net.IP) error {
			addedIPs = append(addedIPs, ip.String())
			return nil
		},
	}

	a, err := NewAllocator(addressManager, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	_, _ = a.Allocate("default/svc-a")
	_, _ = a.Allocate("default/svc-b")
	_, _ = a.Allocate("default/svc-a") // duplicate

	if len(addedIPs) != 2 {
		t.Errorf("AddVIP hook called %d times, want 2", len(addedIPs))
	}
}

func TestAllocate_SkipsAddressInUse(t *testing.T) {
	addressManager := &mockAddressManager{
		AddVIP: func(ip net.IP) error {
			if ip.Equal(net.IPv4(198, 18, 0, 1).To4()) {
				return ErrAddressInUse
			}
			return nil
		},
	}

	a, err := NewAllocator(addressManager, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator failed: %v", err)
	}

	ip, err := a.Allocate("default/svc-a")
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// .1 should have been skipped; first usable VIP is .2.
	expected := net.IPv4(198, 18, 0, 2).To4()
	if !ip.Equal(expected) {
		t.Errorf("got %s, want %s", ip, expected)
	}

	// .1 must be recorded in the skipped set.
	skippedKey := ipToUint32(net.IPv4(198, 18, 0, 1).To4())
	if _, ok := a.skipped[skippedKey]; !ok {
		t.Error("expected 198.18.0.1 to be in the skipped set")
	}
}

func TestReleaseVIP_RemovesFromMaps(t *testing.T) {
	removed := &[]string{}
	var removeMu sync.Mutex
	a, err := NewAllocator(&mockWithRemove{removed: removed, mu: &removeMu}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	ip, err := a.Allocate("default/svc-a")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}

	// Verify it's tracked.
	if a.Lookup("default/svc-a") == nil {
		t.Fatal("expected VIP to be allocated before release")
	}
	if a.ReverseLookup(ip.String()) == "" {
		t.Fatal("expected reverse lookup to work before release")
	}

	// Release it.
	if err := a.ReleaseVIP(ip); err != nil {
		t.Fatalf("ReleaseVIP: %v", err)
	}

	// Both maps must be cleared.
	if a.Lookup("default/svc-a") != nil {
		t.Error("Lookup must return nil after ReleaseVIP")
	}
	if key := a.ReverseLookup(ip.String()); key != "" {
		t.Errorf("ReverseLookup must return empty after ReleaseVIP, got %q", key)
	}
	if len(a.AllAllocations()) != 0 {
		t.Errorf("AllAllocations must be empty after ReleaseVIP, got %d entries", len(a.AllAllocations()))
	}

	// RemoveAddress must have been called once with the released IP.
	removeMu.Lock()
	defer removeMu.Unlock()
	if len(*removed) != 1 {
		t.Errorf("RemoveAddress called %d times, want 1", len(*removed))
	} else if (*removed)[0] != ip.String() {
		t.Errorf("RemoveAddress called with %s, want %s", (*removed)[0], ip.String())
	}
}

// mockWithRemove is an InterfaceAddressManager that records RemoveAddress calls.
type mockWithRemove struct {
	removed *[]string
	mu      *sync.Mutex
}

func (m *mockWithRemove) AddAddress(_ net.IP) error { return nil }
func (m *mockWithRemove) RemoveAddress(ip net.IP) error {
	m.mu.Lock()
	*m.removed = append(*m.removed, ip.String())
	m.mu.Unlock()
	return nil
}

func TestReleaseVIP_UnknownIP_Noop(t *testing.T) {
	a, err := NewAllocator(&mockAddressManager{}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	// Releasing an IP that was never allocated should be a no-op with nil error.
	if err := a.ReleaseVIP(net.IPv4(198, 18, 0, 99).To4()); err != nil {
		t.Errorf("ReleaseVIP on unknown IP returned error: %v", err)
	}
}

func TestReleaseVIP_CleanupAfterRelease(t *testing.T) {
	removed := &[]string{}
	var removeMu sync.Mutex
	a, err := NewAllocator(&mockWithRemove{removed: removed, mu: &removeMu}, "198.18.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	ip1, _ := a.Allocate("ns/svc-1")
	ip2, _ := a.Allocate("ns/svc-2")

	// Release one VIP.
	if err := a.ReleaseVIP(ip1); err != nil {
		t.Fatalf("ReleaseVIP: %v", err)
	}

	// Cleanup should only remove the remaining VIP.
	if err := a.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	removeMu.Lock()
	defer removeMu.Unlock()
	if len(*removed) != 2 {
		t.Errorf("expected 2 RemoveAddress calls (release + cleanup), got %d: %v", len(*removed), *removed)
	}
	if (*removed)[0] != ip1.String() {
		t.Errorf("first remove: got %s, want %s", (*removed)[0], ip1.String())
	}
	if (*removed)[1] != ip2.String() {
		t.Errorf("second remove: got %s, want %s", (*removed)[1], ip2.String())
	}
}
