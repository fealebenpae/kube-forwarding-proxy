package vip

import (
	"fmt"
	"net"
	"os/exec"
)

type ifconfigInterfaceAddressManager struct {
	iface *net.Interface
}

func NewInterfaceAddressManager(iface string) (InterfaceAddressManager, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("find interface %s: %w", iface, err)
	}
	return &ifconfigInterfaceAddressManager{iface: ifi}, nil
}

func (m *ifconfigInterfaceAddressManager) AddAddress(ip net.IP) error {
	addrs, err := m.iface.Addrs()
	if err != nil {
		return fmt.Errorf("list addresses on %s: %w", m.iface.Name, err)
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
			return ErrAddressInUse
		}
	}

	ipStr := ip.String()
	cmd := exec.Command("ifconfig", m.iface.Name, "alias", ipStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s alias %s: %s: %w", m.iface.Name, ipStr, string(output), err)
	}
	return nil
}

func (m *ifconfigInterfaceAddressManager) RemoveAddress(ip net.IP) error {
	ipStr := ip.String()
	cmd := exec.Command("ifconfig", m.iface.Name, "-alias", ipStr)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig %s -alias %s: %s: %w", m.iface.Name, ipStr, string(output), err)
	}
	return nil
}
