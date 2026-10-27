#!/bin/sh
# Install MemSpace from GitHub releases (Linux, macOS).
#
#   curl -fsSL https://raw.githubusercontent.com/chux0519/memspace/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/chux0519/memspace/main/install.sh | sh -s -- --server
#
# Options:
#   --server         install the full binary (server + client) instead of the
#                    client; available for linux-amd64, linux-arm64, darwin-arm64
#   --version VER    a release tag such as v0.1.0 (default: the latest release)
#   --dir DIR        install directory (default: ~/.local/bin)
#
# MEMSPACE_DOWNLOAD_BASE=URL downloads from a mirror of a release's files
# (including SHA256SUMS) instead of GitHub.
#
# The download is verified against the release's SHA256SUMS.
set -eu

REPO="chux0519/memspace"
VERSION="${MEMSPACE_VERSION:-latest}"
DIR="${MEMSPACE_INSTALL_DIR:-$HOME/.local/bin}"
KIND=client

while [ $# -gt 0 ]; do
	case "$1" in
	--server) KIND=server ;;
	--version) VERSION="$2"; shift ;;
	--version=*) VERSION="${1#*=}" ;;
	--dir) DIR="$2"; shift ;;
	--dir=*) DIR="${1#*=}" ;;
	-h | --help) sed -n '2,16p' "$0" 2>/dev/null || true; exit 0 ;;
	*) echo "install.sh: unknown option $1" >&2; exit 2 ;;
	esac
	shift
done

die() { echo "install.sh: $*" >&2; exit 1; }

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported system $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

if [ "$KIND" = server ]; then
	asset="memspace-server-$os-$arch"
	case "$os-$arch" in
	linux-amd64 | linux-arm64 | darwin-arm64) ;;
	*) die "no server build for $os-$arch; build from source (see README)" ;;
	esac
else
	asset="memspace-$os-$arch"
fi

if [ -n "${MEMSPACE_DOWNLOAD_BASE:-}" ]; then
	base="$MEMSPACE_DOWNLOAD_BASE" # a mirror of a release's files
elif [ "$VERSION" = latest ]; then
	base="https://github.com/$REPO/releases/latest/download"
else
	base="https://github.com/$REPO/releases/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "need sha256sum or shasum"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $asset ($VERSION)..."
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$want" ] || die "$asset is not listed in SHA256SUMS"
got="$(sha256 "$tmp/$asset")"
[ "$got" = "$want" ] || die "checksum mismatch for $asset (got $got, want $want)"

mkdir -p "$DIR"
chmod 755 "$tmp/$asset"
mv -f "$tmp/$asset" "$DIR/memspace"
echo "Installed $("$DIR/memspace" --version) to $DIR/memspace"

case ":$PATH:" in
*":$DIR:"*) ;;
*) echo "Note: $DIR is not on PATH. Add it to your shell profile, e.g.:"
   echo "  export PATH=\"$DIR:\$PATH\"" ;;
esac
