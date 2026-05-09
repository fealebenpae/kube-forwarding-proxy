package vip

import "net"

// InterfaceAddressManager manages IP addresses on a network interface.
type InterfaceAddressManager interface {
	AddAddress(ip net.IP) error
	RemoveAddress(ip net.IP) error
}

// Alias-mode constants for NewInterfaceAddressManager.
const (
	// AliasModeAuto means the manager calls ifconfig/ip alias for every VIP.
	// Requires root on macOS, capability NET_ADMIN on Linux.
	AliasModeAuto = "auto"
	// AliasModePreallocated means the VIPs are already aliased to the
	// interface; the manager only verifies presence and never modifies the
	// interface. Allows the daemon to run unprivileged on macOS after a
	// one-time `ifconfig lo0 alias …` sweep performed by the installer.
	AliasModePreallocated = "preallocated"
)
