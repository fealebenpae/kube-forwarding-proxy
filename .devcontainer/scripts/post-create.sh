#!/usr/bin/env bash
# post-create.sh — runs once after the devcontainer is created.
# Sets up file capabilities, installs extra tools, and creates a Kind cluster.
set -euo pipefail

echo "=== Setting file capabilities on capsh ==="
# cap_setpcap  — allows capsh to modify its own inheritable set
# cap_net_admin — for adding VIPs to the loopback interface (netlink)
# cap_net_bind_service — for binding DNS on port 53
sudo setcap cap_net_admin,cap_net_bind_service,cap_setpcap+eip /usr/sbin/capsh

echo "=== Done ==="
