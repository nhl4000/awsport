#!/usr/bin/env bash
# Records the README GIFs against a fake AWS backend (no real AWS calls).
# Requires: go, vhs (https://github.com/charmbracelet/vhs), ttyd, ffmpeg.
#
#   demo/record.sh            # writes assets/cli.gif and assets/tui.gif
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

work=$(mktemp -d)
fake_pid=""
cleanup() {
	[ -n "$fake_pid" ] && kill "$fake_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

mkdir -p "$work/bin" "$work/home/.aws" assets
go build -o "$work/bin/awsport" ./cmd/awsport
go build -o "$work/fakeaws" ./demo/fakeaws
cp demo/bin/aws demo/bin/session-manager-plugin "$work/bin/"
cp demo/acme.team.yaml "$work/acme.team.yaml"
chmod +x "$work/bin/"*

port=4599
"$work/fakeaws" -addr "127.0.0.1:$port" 2>"$work/fakeaws.log" &
fake_pid=$!

# Isolated environment: fake HOME, fake binaries first on PATH, SDK pointed at fakeaws.
: >"$work/home/.aws/config"
env_vars=(
	HOME="$work/home"
	PATH="$work/bin:$PATH"
	AWS_ENDPOINT_URL="http://127.0.0.1:$port"
	AWS_ENDPOINT_URL_SSO="http://127.0.0.1:$port"
	AWS_EC2_METADATA_DISABLED=true
	TERM=xterm-256color
	COLORTERM=truecolor
	AWS_CONFIG_FILE="$work/home/.aws/config"
	AWS_SHARED_CREDENTIALS_FILE="$work/home/.aws/credentials"
	DEMO_SSO_START_URL="https://acme.awsapps.com/start"
	DEMO_SSO_SESSION="acme"
	DEMO_DIR="$work"
)
unset AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
# CI and NO_COLOR make lipgloss/termenv fall back to plain text.
unset AWSPORT_CONFIG_DIR AWSPORT_STATE_DIR XDG_CONFIG_HOME XDG_STATE_HOME NO_COLOR CI

for tape in cli tui; do
	echo "recording assets/$tape.gif"
	env "${env_vars[@]}" vhs "demo/$tape.tape"
done
# Stop any tunnels (and with them the background agent) left by the tapes.
env "${env_vars[@]}" awsport down --all >/dev/null 2>&1 || true
