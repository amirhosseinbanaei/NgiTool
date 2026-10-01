#!/bin/sh
# NgiTool installer — plain POSIX sh.
#
#   curl -fsSL https://raw.githubusercontent.com/amirhosseinbanaei/NgiTool/main/install.sh | sh
#
# Environment:
#   NGITOOL_VERSION        install this release (vX.Y.Z) instead of the latest
#   NGITOOL_PREFIX         install into $NGITOOL_PREFIX/bin (default /usr/local)
#   NGITOOL_DOWNLOAD_BASE  mirror laid out as <base>/<tag>/<asset> (offline installs)
#   NGITOOL_RELEASES_URL   JSON whose first "tag_name" is the release to install
#   GITHUB_TOKEN           sent to api.github.com only, for its rate limit
#
# Flags:
#   --uninstall            remove the binary and the ngt alias
set -eu

REPO=amirhosseinbanaei/NgiTool
BASE=${NGITOOL_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}
BASE=${BASE%/}
RELEASES=${NGITOOL_RELEASES_URL:-https://api.github.com/repos/$REPO/releases/latest}
PREFIX=${NGITOOL_PREFIX:-/usr/local}
BIN_DIR=$PREFIX/bin

if [ -t 1 ] && [ -z "${NO_COLOR+x}" ] && [ "${TERM:-dumb}" != dumb ]; then
	BOLD=$(printf '\033[1m') DIM=$(printf '\033[90m') GREEN=$(printf '\033[32m')
	RED=$(printf '\033[31m') YELLOW=$(printf '\033[33m') CYAN=$(printf '\033[36m') RESET=$(printf '\033[0m')
else
	BOLD='' DIM='' GREEN='' RED='' YELLOW='' CYAN='' RESET=''
fi

say() { printf '%s\n' "$*"; }
step() { printf '%s•%s %s\n' "$CYAN" "$RESET" "$*"; }
ok() { printf '%s✔%s %s\n' "$GREEN" "$RESET" "$*"; }
warn() { printf '%s!%s %s\n' "$YELLOW" "$RESET" "$*"; }
die() {
	printf '%s✖%s %s\n' "$RED" "$RESET" "$*" >&2
	exit 1
}

# Non-root needs a prefix it can write (or create).
check_prefix() {
	[ "$(id -u)" -eq 0 ] && return 0
	d=$BIN_DIR
	while [ ! -d "$d" ]; do d=$(dirname "$d"); done
	[ -w "$d" ] || die "cannot write to $BIN_DIR — run as root (curl … | sudo sh) or set NGITOOL_PREFIX to a directory you own, e.g. NGITOOL_PREFIX=\$HOME/.local"
}

if [ "${1:-}" = "--uninstall" ]; then
	check_prefix
	removed=0
	for f in "$BIN_DIR/ngitool" "$BIN_DIR/ngitool.prev"; do
		if [ -e "$f" ]; then rm -f "$f" && ok "Removed $f" && removed=1; fi
	done
	if [ -L "$BIN_DIR/ngt" ]; then rm -f "$BIN_DIR/ngt" && ok "Removed $BIN_DIR/ngt" && removed=1; fi
	[ "$removed" -eq 1 ] || say "Nothing to remove in $BIN_DIR"
	say "${DIM}Config and state stay in /etc/ngitool, /var/lib/ngitool and /var/cache/ngitool — delete them by hand if you want them gone.${RESET}"
	exit 0
elif [ $# -gt 0 ]; then
	die "unknown argument: $1 (the only flag is --uninstall)"
fi

[ "$(uname -s)" = Linux ] || die "NgiTool runs on Linux only (this is $(uname -s))"
case $(uname -m) in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7* | armv8l) ARCH=armv7 ;;
*) die "no NgiTool build for $(uname -m) — releases exist for amd64, arm64 and armv7" ;;
esac

if command -v curl >/dev/null 2>&1; then
	FETCH=curl
elif command -v wget >/dev/null 2>&1; then
	FETCH=wget
else
	die "needs curl or wget to download — install one of them (apt install curl)"
fi

# fetch URL FILE
fetch() {
	auth=''
	case $1 in https://api.github.com/*) [ -n "${GITHUB_TOKEN:-}" ] && auth="Authorization: Bearer $GITHUB_TOKEN" ;; esac
	if [ "$FETCH" = curl ]; then
		if [ -n "$auth" ]; then curl -fsSL -H "$auth" -o "$2" "$1"; else curl -fsSL -o "$2" "$1"; fi
	else
		if [ -n "$auth" ]; then wget -q --header="$auth" -O "$2" "$1"; else wget -q -O "$2" "$1"; fi
	fi
}

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "needs sha256sum or shasum to verify the download (apt install coreutils)"
fi

check_prefix

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

if [ -n "${NGITOOL_VERSION:-}" ]; then
	TAG=v${NGITOOL_VERSION#v}
else
	step "Finding the latest release"
	fetch "$RELEASES" "$TMP/release.json" ||
		die "cannot read $RELEASES — offline or rate-limited? set NGITOOL_VERSION=vX.Y.Z (and NGITOOL_DOWNLOAD_BASE for a mirror)"
	TAG=$(tr ',' '\n' <"$TMP/release.json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$TAG" ] || die "no release found at $RELEASES — set NGITOOL_VERSION=vX.Y.Z"
fi

ASSET=ngitool_${TAG#v}_linux_${ARCH}.tar.gz
step "Downloading $ASSET ($TAG)"
fetch "$BASE/$TAG/checksums.txt" "$TMP/checksums.txt" || die "cannot download $BASE/$TAG/checksums.txt"
fetch "$BASE/$TAG/$ASSET" "$TMP/$ASSET" || die "cannot download $BASE/$TAG/$ASSET"

WANT=$(awk -v a="$ASSET" '$2 == a || $2 == "*" a { print $1; exit }' "$TMP/checksums.txt")
[ -n "$WANT" ] || die "$ASSET is not listed in checksums.txt — refusing to install an unverified binary"
GOT=$(sha256 "$TMP/$ASSET")
[ "$WANT" = "$GOT" ] || die "checksum mismatch for $ASSET (expected $WANT, got $GOT) — the download is corrupt or was tampered with; nothing was installed"
ok "Verified SHA-256"

tar -xzf "$TMP/$ASSET" -C "$TMP" ngitool || die "$ASSET has no ngitool binary"
mkdir -p "$BIN_DIR"
# Write next to the target and rename over it: a running ngitool keeps working.
cp "$TMP/ngitool" "$BIN_DIR/.ngitool.new.$$"
chmod 755 "$BIN_DIR/.ngitool.new.$$"
mv -f "$BIN_DIR/.ngitool.new.$$" "$BIN_DIR/ngitool"
ln -sf ngitool "$BIN_DIR/ngt"

INSTALLED=$(NGITOOL_NO_UPDATE_CHECK=1 "$BIN_DIR/ngitool" version --json 2>/dev/null |
	sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || true
[ -n "$INSTALLED" ] || die "installed $BIN_DIR/ngitool but it does not run on this machine"

say ""
ok "${BOLD}NgiTool $INSTALLED${RESET} installed"
say "    ${DIM}binary${RESET}  $BIN_DIR/ngitool"
say "    ${DIM}alias ${RESET}  $BIN_DIR/ngt"
case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) warn "$BIN_DIR is not on your PATH — add it: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac
say ""
say "  Next: ${BOLD}ngitool doctor${RESET}"
