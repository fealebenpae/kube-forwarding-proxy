package vip

import (
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
)

type netlinkInterfaceAddressManager struct {
	link netlink.Link
}

// NewInterfaceAddressManager returns an InterfaceAddressManager backed by netlink for the named interface.
func NewInterfaceAddressManager(iface string) (InterfaceAddressManager, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, fmt.Errorf("find interface %s: %w", iface, err)
	}

	return &netlinkInterfaceAddressManager{link: link}, nil
}

func (m *netlinkInterfaceAddressManager) AddAddress(ip net.IP) error {
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
	}
	if err := netlink.AddrAdd(m.link, addr); err != nil {
		if err == syscall.EEXIST {
			return ErrAddressInUse
		}
		return fmt.Errorf("add %s to %s: %w", ip, m.link.Attrs().Name, err)
	}
	return nil
}

func (m *netlinkInterfaceAddressManager) RemoveAddress(ip net.IP) error {
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
	}
	if err := netlink.AddrDel(m.link, addr); err != nil {
		return fmt.Errorf("del %s from %s: %w", ip, m.link.Attrs().Name, err)
	}
	return nil
}
