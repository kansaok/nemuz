#!/bin/sh
# Install nemuz on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/kansaok/nemuz/master/install.sh | sh
#
# Downloads a release archive from GitHub, verifies its SHA-256 against the
# published checksums.txt, and installs the binary. Never installs to a
# system directory by default and never asks for root: nemuz's whole argument
# is a small, sandboxed, non-root binary, and an installer that needed sudo
# to get it onto disk would undercut that on the first command a user runs.
#
# Pass --system to install to /usr/local/bin instead (uses sudo if needed),
# --version vX.Y.Z to pin a release instead of the latest, and --dir <path>
# to choose the install directory yourself.
set -eu

REPO="kansaok/nemuz"
INSTALL_DIR="${NEMUZ_INSTALL_DIR:-$HOME/.local/bin}"
VERSION=""
SYSTEM=0

log()  { printf '%s\n' "$*"; }
die()  { printf 'nemuz: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--system) SYSTEM=1 ;;
	--version) VERSION="$2"; shift ;;
	--dir) INSTALL_DIR="$2"; shift ;;
	-h | --help)
		# Print the leading comment block only — everything up to the first
		# non-comment line — so this stays correct however the script grows,
		# rather than a hardcoded line range going stale the next time a
		# comment line is added or removed above set -eu.
		awk 'NR==1{next} /^#/{sub(/^# ?/,""); print; next} {exit}' "$0"
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
	shift
done
[ "$SYSTEM" = 1 ] && INSTALL_DIR="/usr/local/bin"

need curl
need tar
need mktemp

# --- detect platform, matching goreleaser's {{.Os}}_{{.Arch}} naming ---

os=$(uname -s)
case "$os" in
Linux) os="linux" ;;
Darwin) os="darwin" ;;
*) die "unsupported OS: $os (Windows: use install.ps1 instead)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture: $arch" ;;
esac

# --- resolve the version ---

if [ -z "$VERSION" ]; then
	log "finding the latest release..."
	# The GitHub API doesn't need a token for a public repo's latest release,
	# and grep+cut avoids requiring jq just to read one field.
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		grep '"tag_name"' | head -1 | cut -d'"' -f4)
	[ -n "$VERSION" ] || die "could not determine the latest version"
fi
version_number="${VERSION#v}"

archive="nemuz_${version_number}_${os}_${arch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/$VERSION"

log "installing nemuz $VERSION for $os/$arch"

# --- download and verify ---

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

log "downloading $archive..."
curl -fsSL -o "$tmp/$archive" "$base_url/$archive" ||
	die "download failed — does $VERSION exist for $os/$arch? see https://github.com/$REPO/releases"
curl -fsSL -o "$tmp/checksums.txt" "$base_url/checksums.txt" ||
	die "could not download checksums.txt to verify the archive"

log "verifying checksum..."
expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || die "no checksum listed for $archive"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
else
	die "need sha256sum or shasum to verify the download"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive — expected $expected, got $actual"

# --- install ---

tar -xzf "$tmp/$archive" -C "$tmp" nemuz

if [ "$SYSTEM" = 1 ] && [ ! -w "$INSTALL_DIR" ]; then
	need sudo
	sudo mkdir -p "$INSTALL_DIR"
	sudo install -m 0755 "$tmp/nemuz" "$INSTALL_DIR/nemuz"
else
	mkdir -p "$INSTALL_DIR"
	install -m 0755 "$tmp/nemuz" "$INSTALL_DIR/nemuz"
fi

log "installed to $INSTALL_DIR/nemuz"

case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	log ""
	log "$INSTALL_DIR is not on your PATH. Add it:"
	log "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.bashrc  # or ~/.zshrc"
	log "  export PATH=\"$INSTALL_DIR:\$PATH\"          # for this shell"
	;;
esac

log ""
log "Run 'nemuz doctor' to check what this machine can confine."
