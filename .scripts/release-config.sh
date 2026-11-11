#!/bin/sh
# Presence only. No values, tracing, build, native calls or optional unsigned mode.
set -eu
for value in "${MACOS_SIGN_P12:-}" "${MACOS_SIGN_P12_PASSWORD:-}" \
 "${ASC_ISSUER_ID:-}" "${ASC_KEY_ID:-}" "${ASC_KEY_P8:-}"; do
 case "$value" in
 *[![:space:]]*) ;;
 *) printf '%s\n' 'release-config: all five nonempty signing fields required' >&2; exit 1 ;;
 esac
done
printf '%s\n' 'release-config: all five fields present'
