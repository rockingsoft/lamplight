#!/usr/bin/env bash
set -euo pipefail

# Lamplight writes the complete evaluated request as JSON to stdin.
# stdout must contain only the replacement request JSON.
jq --arg value "$HOOK_VALUE" '.headers["X-Hook"] = $value | .body = "modified"'
