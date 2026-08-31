#!/usr/bin/env bash
set -euo pipefail

CLIENTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY="${CLIENTS_DIR}/deploy.sh"
TEMPLATES="${CLIENTS_DIR}/templates"
FAILURES=0
TEMP_ROOT=""

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  FAILURES=$((FAILURES + 1))
}

assert_contains() {
  local file="$1" needle="$2" description="$3"
  if ! grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    fail "$description: expected '$needle' in $(basename "$file")"
  fi
}

assert_lacks() {
  local file="$1" needle="$2" description="$3"
  if grep -F -- "$needle" "$file" >/dev/null 2>&1; then
    fail "$description: did not expect '$needle' in $(basename "$file")"
  fi
}

assert_mode() {
  local file="$1" expected="$2"
  local actual
  actual="$(stat -c '%a' "$file" 2>/dev/null || stat -f '%Lp' "$file")"
  test "$actual" = "$expected" || fail "mode of $file is ${actual}, expected ${expected}"
}

cleanup() {
  if test -n "$TEMP_ROOT"; then
    rm -rf "$TEMP_ROOT"
  fi
}

trap cleanup EXIT

for required in "$DEPLOY" "$TEMPLATES/env.sh" "$TEMPLATES/print-key.sh" \
  "$TEMPLATES/claude.settings.local.json" "$TEMPLATES/codex.provider.toml" \
  "$TEMPLATES/grok.cliproxy.toml" "$CLIENTS_DIR/lib/apply.py" "$CLIENTS_DIR/README.md"; do
  test -f "$required" || fail "missing ${required}"
done

if test "$FAILURES" -ne 0; then
  exit 1
fi

# Tracked templates must stay secret-free and parameterized.
for template in "$TEMPLATES"/*; do
  assert_lacks "$template" 'experimental_bearer_token' "template secret field"
  assert_lacks "$template" '192.168.60.32' "hardcoded gateway host"
done
for template in "$TEMPLATES/env.sh" "$TEMPLATES/claude.settings.local.json" \
  "$TEMPLATES/codex.provider.toml" "$TEMPLATES/grok.cliproxy.toml"; do
  assert_contains "$template" 'GATEWAY_URL' "gateway placeholder"
done
assert_contains "$TEMPLATES/env.sh" 'KEY_FILE' "key filename placeholder"
assert_contains "$TEMPLATES/print-key.sh" 'KEY_FILE' "key filename placeholder"
assert_contains "$TEMPLATES/claude.settings.local.json" 'API_KEY_HELPER' "helper placeholder"
assert_contains "$TEMPLATES/codex.provider.toml" '[model_providers.cliproxy]' "codex provider"
assert_contains "$TEMPLATES/grok.cliproxy.toml" 'env_key = "CLIPROXY_API_KEY"' "grok env key"
assert_lacks "$CLIENTS_DIR/lib/apply.py" '192.168.60.32:8317/v1' "apply helper hardcodes only the default origin"

if git -C "$CLIENTS_DIR/.." ls-files | grep -E '(^|/)[^/]+\.key$' >/dev/null 2>&1; then
  fail "tracked client key file"
fi

TEMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/cliproxy-client-deploy.XXXXXX")"
HOME_DIR="${TEMP_ROOT}/home"
KEY_FILE="${TEMP_ROOT}/source.key"
mkdir -p "$HOME_DIR/.claude" "$HOME_DIR/.codex" "$HOME_DIR/.grok"
printf '%s' '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' > "$KEY_FILE"
chmod 600 "$KEY_FILE"

printf '%s\n' '{"theme":"dark"}' > "$HOME_DIR/.claude/settings.local.json"
cat > "$HOME_DIR/.codex/config.toml" <<'EOF'
model = "gpt-5.6-sol"
[mcp_servers.lsp]
command = "npx"
EOF
cat > "$HOME_DIR/.grok/config.toml" <<'EOF'
[ui]
yolo = false
EOF
cat > "$HOME_DIR/.bashrc" <<'EOF'
# If not running interactively, don't do anything
case $- in
    *i*) ;;
      *) return;;
esac
EOF

if ! "$DEPLOY" --home "$HOME_DIR" --key-file "$KEY_FILE" --client-name testhost \
  --gateway-url 'http://192.0.2.10:8317' > "${TEMP_ROOT}/deploy.out"; then
  fail "first deploy exited non-zero"
fi
if grep -F -- '0123456789abcdef' "${TEMP_ROOT}/deploy.out" >/dev/null 2>&1; then
  fail "deploy printed the client key"
fi
assert_contains "${TEMP_ROOT}/deploy.out" 'applied CLIProxyAPI client routing' "deploy success line"

assert_mode "$HOME_DIR/.config/cliproxy/testhost.key" 600
assert_mode "$HOME_DIR/.config/cliproxy/env.sh" 600
assert_mode "$HOME_DIR/.config/cliproxy/print-key.sh" 700
assert_mode "$HOME_DIR/.claude/settings.local.json" 600
assert_mode "$HOME_DIR/.codex/config.toml" 600

assert_contains "$HOME_DIR/.config/cliproxy/env.sh" 'http://192.0.2.10:8317' "rendered gateway"
assert_contains "$HOME_DIR/.config/cliproxy/env.sh" 'testhost.key' "rendered key name"
assert_contains "$HOME_DIR/.claude/settings.local.json" '"theme": "dark"' "preserved Claude settings"
assert_contains "$HOME_DIR/.claude/settings.local.json" 'ANTHROPIC_BASE_URL' "Claude gateway env"
assert_contains "$HOME_DIR/.codex/config.toml" 'model_provider = "cliproxy"' "Codex provider selection"
assert_contains "$HOME_DIR/.codex/config.toml" '[mcp_servers.lsp]' "preserved Codex MCP"
assert_contains "$HOME_DIR/.codex/config.toml" 'experimental_bearer_token =' "Codex injected bearer"
assert_contains "$HOME_DIR/.grok/config.toml" '[ui]' "preserved Grok UI"
assert_contains "$HOME_DIR/.grok/config.toml" 'models_base_url = "http://192.0.2.10:8317/v1"' "Grok gateway"
assert_contains "$HOME_DIR/.bashrc" 'cliproxy/env.sh' "bashrc hook before interactive return"
assert_contains "$HOME_DIR/.profile" 'cliproxy/env.sh' "profile hook"
assert_contains "$HOME_DIR/.zshenv" 'cliproxy/env.sh' "zshenv hook"

CLIPROXY_API_KEY=""
ANTHROPIC_BASE_URL=""
HOME="$HOME_DIR"
# shellcheck disable=SC1091
. "$HOME_DIR/.config/cliproxy/env.sh"
test "${ANTHROPIC_BASE_URL:-}" = 'http://192.0.2.10:8317' || fail "env.sh did not export ANTHROPIC_BASE_URL"
test "${#CLIPROXY_API_KEY}" -eq 64 || fail "env.sh did not load the client key"

if ! "$DEPLOY" --home "$HOME_DIR" --key-file "$KEY_FILE" --client-name testhost \
  --gateway-url 'http://192.0.2.10:8317' >/dev/null; then
  fail "idempotent deploy exited non-zero"
fi
hook_count="$(grep -c 'cliproxy/env.sh' "$HOME_DIR/.bashrc" || true)"
test "$hook_count" -eq 1 || fail "bashrc hook was duplicated (${hook_count})"

chmod 644 "$KEY_FILE"
if "$DEPLOY" --home "$HOME_DIR" --key-file "$KEY_FILE" --client-name testhost \
  --gateway-url 'http://192.0.2.10:8317' >/dev/null 2>"${TEMP_ROOT}/mode.err"; then
  fail "deploy accepted a key file that was not mode 0600"
else
  assert_contains "${TEMP_ROOT}/mode.err" 'mode 0600' "mode-0600 rejection"
fi

if test "$FAILURES" -ne 0; then
  exit 1
fi
printf '%s\n' 'PASS: client templates stay secret-free and deploy idempotently'
