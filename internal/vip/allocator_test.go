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
