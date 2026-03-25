// Package vip provides virtual IP allocation from a private CIDR range.
package vip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
)

// Allocator manages virtual IP allocation for Kubernetes services.
type Allocator struct {
	mu          sync.RWMutex
	addrManager InterfaceAddressManager
	network     *net.IPNet
	base        uint32
	max         uint32
	next        uint32
	assigned    map[string]net.IP   // serviceKey -> VIP
	reverse     map[string]string   // VIP string -> serviceKey
	skipped     map[uint32]struct{} // addresses rejected by AddToInterface (already in use)
}

// NewAllocator creates a VIP allocator for the given CIDR range.
func NewAllocator(addrManager InterfaceAddressManager, cidr string) (*Allocator, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parsing CIDR %s: %w", cidr, err)
	}

	base := ipToUint32(network.IP)
	ones, bits := network.Mask.Size()
	size := uint32(1) << uint(bits-ones)

	return &Allocator{
		addrManager: addrManager,
		network:     network,
		base:        base + 1,
		max:         base + size - 1,
		next:        base + 1,
		assigned:    make(map[string]net.IP),
		reverse:     make(map[string]string),
		skipped:     make(map[uint32]struct{}),
	}, nil
}

// Allocate returns the VIP for a service, allocating a new one if needed.
// serviceKey should be in the form "namespace/serviceName" or "context/namespace/serviceName".
func (a *Allocator) Allocate(serviceKey string) (net.IP, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if ip, ok := a.assigned[serviceKey]; ok {
		return ip, nil
	}

	for {
		if a.next >= a.max {
			return nil, fmt.Errorf("VIP CIDR exhausted: no more addresses available")
		}

		ip := uint32ToIP(a.next)
		a.next++

		if err := a.addrManager.AddAddress(ip); err != nil {
			if errors.Is(err, ErrAddressInUse) {
				// Address already in use on the interface; skip it permanently.
				a.skipped[ipToUint32(ip)] = struct{}{}
				continue
			}
			return nil, fmt.Errorf("adding VIP %s: %w", ip, err)
		}

		a.assigned[serviceKey] = ip
		a.reverse[ip.String()] = serviceKey
		return ip, nil
	}
}

// Lookup returns the VIP for a service, or nil if not allocated.
func (a *Allocator) Lookup(serviceKey string) net.IP {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.assigned[serviceKey]
}

// ReverseLookup returns the service key for a VIP, or empty string if unknown.
func (a *Allocator) ReverseLookup(ip string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reverse[ip]
}

// AllAllocations returns a copy of all current allocations.
func (a *Allocator) AllAllocations() map[string]net.IP {
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make(map[string]net.IP, len(a.assigned))
	for k, v := range a.assigned {
		result[k] = v
	}
	return result
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}

// ReleaseVIP removes a single allocated VIP from the allocator's tracking and
// from the network interface. It is a no-op if the IP was not allocated by
// this allocator.
func (a *Allocator) ReleaseVIP(ip net.IP) error {
	addrStr := ip.String()

	a.mu.Lock()
	serviceKey, ok := a.reverse[addrStr]
	if !ok {
		a.mu.Unlock()
		return nil
	}
	delete(a.assigned, serviceKey)
	delete(a.reverse, addrStr)
	a.mu.Unlock()

	if err := a.addrManager.RemoveAddress(ip); err != nil {
		return fmt.Errorf("removing VIP %s: %w", ip, err)
	}
	return nil
}

// Cleanup removes all allocated VIPs by calling RemoveAddress for each one.
// Errors are collected but all VIPs are attempted.
func (a *Allocator) Cleanup() error {
	a.mu.RLock()
	allocs := make(map[string]net.IP, len(a.assigned))
	for k, v := range a.assigned {
		allocs[k] = v
	}
	a.mu.RUnlock()

	var errs []error
	for key, ip := range allocs {
		if err := a.addrManager.RemoveAddress(ip); err != nil {
			errs = append(errs, fmt.Errorf("removing VIP %s for %s: %w", ip, key, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("VIP cleanup errors: %v", errs)
	}
	return nil
}
