#!/bin/sh
# Install awsport from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/nhl4000/awsport/main/install.sh | sh
#
# Environment:
#   AWSPORT_VERSION  release tag to install (default: latest), e.g. v0.3.0
#   INSTALL_DIR      target directory (default: /usr/local/bin as root, else ~/.local/bin)
#   GITHUB_TOKEN     token for private repositories / higher API rate limits
set -eu

REPO="nhl4000/awsport"
VERSION="${AWSPORT_VERSION:-latest}"
TOKEN="${GITHUB_TOKEN:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN*) die "on Windows download awsport_windows_<arch>.zip from https://github.com/$REPO/releases" ;;
	*) die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

# fetch URL OUTFILE [ACCEPT]: download, sending the token when set.
fetch() {
	_url=$1 _out=$2 _accept=${3:-}
	if have curl; then
		set -- -fsSL -o "$_out"
		[ -n "$_accept" ] && set -- "$@" -H "Accept: $_accept"
		[ -n "$TOKEN" ] && set -- "$@" -H "Authorization: Bearer $TOKEN"
		curl "$@" "$_url" || die "download failed: $_url"
	elif have wget; then
		[ -n "$TOKEN" ] && die "GITHUB_TOKEN requires curl"
		set -- -q -O "$_out"
		[ -n "$_accept" ] && set -- "$@" --header "Accept: $_accept"
		wget "$@" "$_url" || die "download failed: $_url"
	else
		die "curl or wget is required"
	fi
}

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

# asset_api_url RELEASE_JSON NAME: API URL of the named release asset.
asset_api_url() {
	# shellcheck disable=SC2020 # each of , { } becomes a newline
	tr ',{}' '\n\n\n' <"$1" | awk -v want="$2" '
		/"url": *"https:\/\/api\.github\.com\/repos\/.*\/releases\/assets\/[0-9]+"/ {
			u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u)
		}
		/"name": *"/ {
			n = $0; sub(/.*"name": *"/, "", n); sub(/".*/, "", n)
			if (n == want && u != "") { print u; exit }
		}'
}

OS=$(detect_os)
ARCH=$(detect_arch)
ASSET="awsport_${OS}_${ARCH}.tar.gz"

if [ -z "${INSTALL_DIR:-}" ]; then
	if [ "$(id -u)" -eq 0 ]; then
		INSTALL_DIR=/usr/local/bin
	else
		INSTALL_DIR="$HOME/.local/bin"
	fi
fi

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t awsport)
trap 'rm -rf "$TMP"' EXIT
trap 'exit 130' INT TERM

if [ -n "$TOKEN" ]; then
	# Private repositories only serve assets through the API.
	if [ "$VERSION" = latest ]; then
		api="https://api.github.com/repos/$REPO/releases/latest"
	else
		api="https://api.github.com/repos/$REPO/releases/tags/$VERSION"
	fi
	fetch "$api" "$TMP/release.json" "application/vnd.github+json"
	archive_url=$(asset_api_url "$TMP/release.json" "$ASSET")
	sums_url=$(asset_api_url "$TMP/release.json" checksums.txt)
	[ -n "$archive_url" ] || die "release has no asset $ASSET"
	[ -n "$sums_url" ] || die "release has no checksums.txt"
	accept="application/octet-stream"
else
	if [ "$VERSION" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$VERSION"
	fi
	archive_url="$base/$ASSET"
	sums_url="$base/checksums.txt"
	accept=""
fi

say "Downloading $ASSET ($VERSION)…"
fetch "$archive_url" "$TMP/$ASSET" "$accept"
fetch "$sums_url" "$TMP/checksums.txt" "$accept"

want=$(awk -v name="$ASSET" '$2 == name || $2 == "*" name {print $1; exit}' "$TMP/checksums.txt")
[ -n "$want" ] || die "checksums.txt has no entry for $ASSET"
got=$(sha256_of "$TMP/$ASSET")
[ "$(printf '%s' "$want" | tr 'A-F' 'a-f')" = "$got" ] || die "checksum mismatch for $ASSET (got $got, want $want)"
say "Checksum verified."

tar -xzf "$TMP/$ASSET" -C "$TMP"
[ -f "$TMP/awsport" ] || die "archive does not contain the awsport binary"

mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR (set INSTALL_DIR or run with sudo)"
cp "$TMP/awsport" "$INSTALL_DIR/.awsport.tmp.$$" || die "cannot write to $INSTALL_DIR (set INSTALL_DIR or run with sudo)"
chmod 0755 "$INSTALL_DIR/.awsport.tmp.$$"
mv -f "$INSTALL_DIR/.awsport.tmp.$$" "$INSTALL_DIR/awsport"

say "Installed awsport to $INSTALL_DIR/awsport"
case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*) say "Note: $INSTALL_DIR is not on your PATH; add it to your shell profile." ;;
esac
say "Run 'awsport' to get started."
