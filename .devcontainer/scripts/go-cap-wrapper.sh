#!/bin/bash
# Wrapper for the Go toolchain that ensures CAP_NET_ADMIN and
# CAP_NET_BIND_SERVICE are available as ambient capabilities.
# Used by VS Code's Go extension (go.alternateTools.go) so that
# "Run Test" / "Debug Test" work without sudo.

# Check if ambient caps are already raised (bits 10+12 = 0x1400).
_capamb=$(grep -oP 'CapAmb:\s*\K\S+' /proc/self/status 2>/dev/null)
if [ "$(( 0x${_capamb:-0} & 0x1400 ))" -eq "$(( 0x1400 ))" ]; then
    exec /usr/local/go/bin/go "$@"
fi

if /usr/sbin/capsh --inh=cap_net_admin --addamb=cap_net_admin --print >/dev/null 2>&1; then
    exec /usr/sbin/capsh \
        --inh=cap_net_admin,cap_net_bind_service \
        --addamb=cap_net_admin,cap_net_bind_service \
        -- -c 'exec /usr/local/go/bin/go "$@"' go "$@"
fi

exec /usr/local/go/bin/go "$@"
