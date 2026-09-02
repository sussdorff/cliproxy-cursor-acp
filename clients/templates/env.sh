# CLIProxyAPI client env for Claude Code / Codex / Grok.
# Cursor Agent stays native and is not redirected here.
# Do not echo CLIPROXY_API_KEY.
# GATEWAY_URL and KEY_FILE are substituted by clients/deploy.sh.
if test -r "${HOME}/.config/cliproxy/KEY_FILE"; then
  CLIPROXY_API_KEY="$(tr -d '\n' < "${HOME}/.config/cliproxy/KEY_FILE")"
  export CLIPROXY_API_KEY
  export ANTHROPIC_BASE_URL="GATEWAY_URL"
  # Claude Code rejects pairing ANTHROPIC_AUTH_TOKEN with apiKeyHelper.
  # The helper in settings.local.json is the Claude credential; this token
  # is only for Codex/Grok via CLIPROXY_API_KEY.
  unset ANTHROPIC_AUTH_TOKEN
fi
