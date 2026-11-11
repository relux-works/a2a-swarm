#!/bin/sh
# Only this job's fixed private directory, never an operator/default keychain.
set -eu
case "${RUNNER_TEMP:-}" in /*) ;; *) exit 1 ;; esac
private="$RUNNER_TEMP/a2a-release-private"
if [ -L "$private" ]; then exit 1; fi
if [ -f "$private/signing.keychain-db" ]; then
 /usr/bin/security delete-keychain "$private/signing.keychain-db" >/dev/null 2>&1 || exit 1
fi
rm -rf -- "$private"
test ! -e "$private"
