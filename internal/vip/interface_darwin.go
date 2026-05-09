package vip

import (
	"fmt"
	"net"
	"os/exec"
)

type ifconfigInterfaceAddressManager struct {
	iface        *net.Interface
	preallocated bool
}

// NewInterfaceAddressManager returns an InterfaceAddressManager that manages
// IP aliases on the given interface using ifconfig.
//
// aliasMode controls the privilege model:
//
//	"auto"          (default) — call `ifconfig <iface> alias <ip>` per
//	                             allocation; requires root on macOS.
//	"preallocated"            — assume the VIPs are already aliased to
//	                             the interface (e.g. by `k8s-service-proxy install`)
//	                             and never invoke ifconfig. AddAddress just
//	                             verifies presence; RemoveAddress is a no-op so
//	                             the pool persists across restarts.
func NewInterfaceAddressManager(iface, aliasMode string) (InterfaceAddressManager, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("find interface %s: %w", iface, err)
	}
	return &ifconfigInterfaceAddressManager{
		iface:        ifi,
		preallocated: aliasMode == AliasModePreallocated,
	}, nil
}

func (m *ifconfigInterfaceAddressManager) AddAddress(ip net.IP) error {
	present, err := m.hasAddress(ip)
	if err != nil {
		return err
	}

	if m.preallocated {
		// In preallocated mode the installer is responsible for adding the
		// alias; if it's not there the VIP is unusable. Surface as an
		// in-use-style error so the allocator skips it and tries the next IP.
		if !present {
			return fmt.Errorf("VIP %s not pre-aliased on %s (run `k8s-service-proxy install` or expand the pool): %w",
				ip, m.iface.Name, ErrAddressInUse)
		}
		return nil
	}

	if present {
		return ErrAddressInUse
	}

	ipStr := ip.String()
	cmd := exec.Command("ifconfig", m.iface.Name, "alias", ipStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s alias %s: %s: %w", m.iface.Name, ipStr, string(output), err)
	}
	return nil
}

func (m *ifconfigInterfaceAddressManager) RemoveAddress(ip net.IP) error {
	if m.preallocated {
		// Pool addresses are managed by the installer and persist across
		// daemon restarts; do not remove them on cleanup.
		return nil
	}
	ipStr := ip.String()
	cmd := exec.Command("ifconfig", m.iface.Name, "-alias", ipStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s -alias %s: %s: %w", m.iface.Name, ipStr, string(output), err)
	}
	return nil
}

func (m *ifconfigInterfaceAddressManager) hasAddress(ip net.IP) (bool, error) {
	addrs, err := m.iface.Addrs()
	if err != nil {
		return false, fmt.Errorf("list addresses on %s: %w", m.iface.Name, err)
	}
	for _, a := range addrs {
		var existing net.IP
		switch v := a.(type) {
		case *net.IPNet:
			existing = v.IP
		case *net.IPAddr:
			existing = v.IP
		}
		if existing.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}
