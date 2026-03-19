package vip

import "net"

// InterfaceAddressManager manages IP addresses on a network interface.
type InterfaceAddressManager interface {
	AddAddress(ip net.IP) error
	RemoveAddress(ip net.IP) error
}
