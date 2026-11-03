#!/bin/sh
set -eu

: "${PASSWORD:?Senha da IDE ausente.}"
mkdir -p "$HOME/project" "$HOME/.local/share/code-server/User"
if [ ! -e "$HOME/.local/share/code-server/User/settings.json" ]; then
    cp /etc/code-server/settings.json "$HOME/.local/share/code-server/User/settings.json"
fi
exec code-server --config /etc/code-server/config.yaml \
    --disable-telemetry --disable-update-check \
    --extensions-dir /opt/code-extensions "$HOME/project"
