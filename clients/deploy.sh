#!/usr/bin/env bash
set -euo pipefail

# Install Claude Code, Codex, and Grok routing against a CLIProxyAPI gateway.
# The client key is read from a mode-0600 file and never printed.

readonly CLIENTS_DIR="$(cd "$(dirname "$0")" && pwd)"
readonly APPLY="${CLIENTS_DIR}/lib/apply.py"
readonly TEMPLATES="${CLIENTS_DIR}/templates"
DEFAULT_GATEWAY_URL="http://192.168.60.32:8317"
HOME_DIR="${HOME}"
GATEWAY_URL="${DEFAULT_GATEWAY_URL}"
KEY_FILE=""
CLIENT_NAME=""

die() {
  printf '%s\n' "$1" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Install Claude Code, Codex, and Grok client routing for CLIProxyAPI.

Usage:
  clients/deploy.sh --key-file <mode-0600-key> --client-name <name> [options]

Required:
  --key-file PATH       Existing mode-0600 client key. Copied to
                        ~/.config/cliproxy/<client-name>.key
  --client-name NAME    Local key basename, for example macbook or yakushido

Optional:
  --gateway-url URL     CLIProxyAPI origin (default: http://192.168.60.32:8317)
  --home PATH           Target home directory (default: $HOME)
  -h, --help            Show this help

The script never prints the key. Cursor Agent is left native.
EOF
}

while test $# -gt 0; do
  case "$1" in
    --key-file)
      test $# -ge 2 || die "--key-file requires a path"
      KEY_FILE="$2"
      shift 2
      ;;
    --client-name)
      test $# -ge 2 || die "--client-name requires a value"
      CLIENT_NAME="$2"
      shift 2
      ;;
    --gateway-url)
      test $# -ge 2 || die "--gateway-url requires a URL"
      GATEWAY_URL="$2"
      shift 2
      ;;
    --home)
      test $# -ge 2 || die "--home requires a path"
      HOME_DIR="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

test -n "$KEY_FILE" || die "missing --key-file"
test -n "$CLIENT_NAME" || die "missing --client-name"
case "$CLIENT_NAME" in
  *[!A-Za-z0-9_-]*)
    die "--client-name must be [A-Za-z0-9_-]"
    ;;
esac
test -f "$APPLY" || die "missing apply helper: ${APPLY}"
test -d "$TEMPLATES" || die "missing templates: ${TEMPLATES}"

python3 "$APPLY" \
  --home "$HOME_DIR" \
  --templates "$TEMPLATES" \
  --gateway-url "$GATEWAY_URL" \
  --key-file "$KEY_FILE" \
  --client-name "$CLIENT_NAME"
