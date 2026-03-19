package vip

import "errors"

// ErrAddressInUse is returned by AddToInterface when the IP address is already
// configured on the interface. The allocator treats this as a signal to skip
// the address and try the next one.
var ErrAddressInUse = errors.New("address already in use on interface")
