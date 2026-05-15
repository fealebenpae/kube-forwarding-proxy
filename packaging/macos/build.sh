#!/usr/bin/env bash
# build.sh — Build the macOS arm64 pkg installer for k8s-service-proxy.
#
# Usage:
#   bash packaging/macos/build.sh [VERSION]
#
# If VERSION is not supplied it is derived from `git describe --tags --dirty`.
# The script must be run from the root of the repository.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

cd "$REPO_ROOT"

# ---------------------------------------------------------------------------
# Version
# ---------------------------------------------------------------------------
VERSION="${1:-}"
if [[ -z "$VERSION" ]]; then
    VERSION="$(git describe --tags --dirty 2>/dev/null || echo "0.0.0-dev")"
fi
# Strip leading 'v' for pkgbuild (requires a dotted-decimal version).
PKG_VERSION="${VERSION#v}"

echo "Building k8s-proxy $VERSION ($PKG_VERSION) for darwin/arm64"

# ---------------------------------------------------------------------------
# Directories
# ---------------------------------------------------------------------------
STAGING="$REPO_ROOT/.pkg-staging"
OUTPUT="k8s-proxy-${VERSION}-arm64.pkg"
# Temporary directory used as --package-path for productbuild so that
# distribution.xml can reference 'component.pkg' by its bare filename.
PKG_DIR="$(mktemp -d)"
COMPONENT="$PKG_DIR/component.pkg"

cleanup() {
    rm -rf "$STAGING" "$PKG_DIR"
}
trap cleanup EXIT

rm -rf "$STAGING"

# ---------------------------------------------------------------------------
# Compile
# ---------------------------------------------------------------------------
BINARY="$STAGING/usr/local/bin/k8s-service-proxy"
mkdir -p "$(dirname "$BINARY")"

echo "Compiling binary..."
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o "$BINARY" ./cmd/proxy

chmod 755 "$BINARY"

# ---------------------------------------------------------------------------
# LaunchDaemon plist
# ---------------------------------------------------------------------------
PLIST_DST="$STAGING/Library/LaunchDaemons/io.github.fealebenpae.k8s-proxy.plist"
mkdir -p "$(dirname "$PLIST_DST")"
cp "$SCRIPT_DIR/com.k8s-proxy.plist" "$PLIST_DST"
chmod 644 "$PLIST_DST"

# ---------------------------------------------------------------------------
# macOS resolver entry
# /etc on macOS is a symlink to /private/etc; use the real path in the payload.
# ---------------------------------------------------------------------------
RESOLVER_DST="$STAGING/private/etc/resolver/svc.cluster.local"
mkdir -p "$(dirname "$RESOLVER_DST")"
cat > "$RESOLVER_DST" <<'EOF'
# DNS resolver configuration for Kubernetes cluster services.
# Queries for *.svc.cluster.local are forwarded to the k8s-proxy DNS server.
nameserver 127.0.0.1
port 11617
EOF
chmod 644 "$RESOLVER_DST"

# ---------------------------------------------------------------------------
# Build component package
# ---------------------------------------------------------------------------
echo "Running pkgbuild..."
pkgbuild \
    --root "$STAGING" \
    --scripts "$SCRIPT_DIR/scripts" \
    --identifier "io.github.fealebenpae.k8s-proxy" \
    --version "$PKG_VERSION" \
    --install-location "/" \
    "$COMPONENT"

# ---------------------------------------------------------------------------
# Build distribution package
# ---------------------------------------------------------------------------
echo "Running productbuild..."
productbuild \
    --distribution "$SCRIPT_DIR/distribution.xml" \
    --package-path "$PKG_DIR" \
    "$OUTPUT"

echo "Done: $OUTPUT"
