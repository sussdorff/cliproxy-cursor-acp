# CLIProxyAPI client env for Claude Code / Codex / Grok.
# Cursor Agent stays native and is not redirected here.
# Do not echo CLIPROXY_API_KEY.
# GATEWAY_URL and KEY_FILE are substituted by clients/deploy.sh.
if test -r "${HOME}/.config/cliproxy/KEY_FILE"; then
  CLIPROXY_API_KEY="$(tr -d '\n' < "${HOME}/.config/cliproxy/KEY_FILE")"
  export CLIPROXY_API_KEY
  export ANTHROPIC_BASE_URL="GATEWAY_URL"
  export ANTHROPIC_AUTH_TOKEN="${CLIPROXY_API_KEY}"
fi
