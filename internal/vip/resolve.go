package vip

import (
	"fmt"
	"net"
)

// ResolveIface resolves an IFACE value (either an interface name like "eth0" or
// a raw IPv4 address like "192.168.1.10") into a bind IP and the owning
// interface name.
//
// When value is an interface name, the first unicast IPv4 address on that
// interface is returned as the bind IP.
//
// When value is an IP address, the interface that owns that address is located
// and the IP is returned verbatim as the bind IP.
func ResolveIface(value string) (bindIP net.IP, ifaceName string, err error) {
	if ip := net.ParseIP(value); ip != nil {
		// value is a raw IP address — find which interface owns it.
		ip = ip.To4()
		if ip == nil {
			return nil, "", fmt.Errorf("IFACE address %q is not an IPv4 address", value)
		}
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, "", fmt.Errorf("listing network interfaces: %w", err)
		}
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ifIP net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ifIP = v.IP.To4()
				case *net.IPAddr:
					ifIP = v.IP.To4()
				}
				if ifIP != nil && ifIP.Equal(ip) {
					return ip, iface.Name, nil
				}
			}
		}
		return nil, "", fmt.Errorf("no interface found with address %s", value)
	}

	// value is an interface name — find its first IPv4 address.
	iface, err := net.InterfaceByName(value)
	if err != nil {
		return nil, "", fmt.Errorf("interface %q not found: %w", value, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, "", fmt.Errorf("listing addresses for interface %q: %w", value, err)
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP.To4()
		case *net.IPAddr:
			ip = v.IP.To4()
		}
		if ip != nil {
			return ip, iface.Name, nil
		}
	}
	return nil, "", fmt.Errorf("interface %q has no IPv4 address", value)
}
