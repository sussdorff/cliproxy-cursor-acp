#!/bin/sh
set -eu
# KEY_FILE is substituted by clients/deploy.sh.
key_file="${HOME}/.config/cliproxy/KEY_FILE"
test -f "$key_file"
tr -d '\n' < "$key_file"
