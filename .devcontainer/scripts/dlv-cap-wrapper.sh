#!/bin/bash
# Wrapper for the Delve debugger that ensures CAP_NET_ADMIN and
# CAP_NET_BIND_SERVICE are available as ambient capabilities.
# Used by VS Code's Go extension (go.alternateTools.dlv) so that
# "Debug Test" works without sudo.

# Resolve the real dlv binary (installed by the Go extension into GOPATH/bin).
REAL_DLV="${GOPATH:-/go}/bin/dlv"
if [ ! -x "$REAL_DLV" ]; then
    REAL_DLV="/go/bin/dlv"
fi

# Check if ambient caps are already raised (bits 10+12 = 0x1400).
_capamb=$(grep -oP 'CapAmb:\s*\K\S+' /proc/self/status 2>/dev/null)
if [ "$(( 0x${_capamb:-0} & 0x1400 ))" -eq "$(( 0x1400 ))" ]; then
    exec "$REAL_DLV" "$@"
fi

if /usr/sbin/capsh --inh=cap_net_admin --addamb=cap_net_admin --print >/dev/null 2>&1; then
    exec /usr/sbin/capsh \
        --inh=cap_net_admin,cap_net_bind_service \
        --addamb=cap_net_admin,cap_net_bind_service \
        -- -c "exec \"$REAL_DLV\" \"\$@\"" dlv "$@"
fi

exec "$REAL_DLV" "$@"
