#!/bin/bash
# Login-shell wrapper that raises CAP_NET_ADMIN and CAP_NET_BIND_SERVICE as
# ambient capabilities so that every child process (go test, go run, dlv, …)
# inherits them automatically — no sudo required.
#
# Falls back to a plain bash shell when capsh lacks the required file
# capabilities (e.g. during postCreateCommand before setcap has been run).

# Check if ambient caps are already raised (bits 10+12 = 0x1400).
_capamb=$(grep -oP 'CapAmb:\s*\K\S+' /proc/self/status 2>/dev/null)
if [ "$(( 0x${_capamb:-0} & 0x1400 ))" -eq "$(( 0x1400 ))" ]; then
    exec /bin/bash "$@"
fi

# Quick probe: can capsh actually raise the caps we need?
if /usr/sbin/capsh --inh=cap_net_admin --addamb=cap_net_admin --print >/dev/null 2>&1; then
    exec /usr/sbin/capsh \
        --inh=cap_net_admin,cap_net_bind_service \
        --addamb=cap_net_admin,cap_net_bind_service \
        -- -c 'exec /bin/bash "$@"' bash "$@"
fi

# Fallback — capsh cannot raise caps yet.
exec /bin/bash "$@"
