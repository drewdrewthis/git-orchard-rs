#!/usr/bin/env bash
NODE="$(command -v node)"; [ -z "$NODE" ] && exit 0
exec "$NODE" "${CLAUDE_PLUGIN_ROOT}/hooks/lib.mjs" compile
