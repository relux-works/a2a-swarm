#!/bin/bash
# Throwaway self-signed code-signing identity for the native signing-keychain
# control. It is never a release credential. It changes no trust setting and
# touches no login keychain. (Removing a trust setting hangs on a headless
# runner, so none is made.)
#   signing-keychain-fixture.sh DIR            create identity.p12/identity.pass in DIR
#   signing-keychain-fixture.sh --cleanup DIR  remove DIR and check that no fixture
#                                              keychain is left in the user search list
set -euo pipefail
# bounded SECONDS CMD...: run CMD, killed by SIGALRM after SECONDS, so that no
# trust-settings or keychain call can hang the job (exit 142 on timeout).
bounded() {
	/usr/bin/perl -e 'alarm shift; exec @ARGV or exit 127' "$@"
}
if [ "${1:-}" = "--cleanup" ]; then
	dir=${2:?usage: signing-keychain-fixture.sh --cleanup DIR}
	status=0
	rm -rf "$dir" || status=1
	if [ -e "$dir" ]; then status=1; fi
	if bounded 30 /usr/bin/security list-keychains -d user | grep -q 'control.keychain-db'; then status=1; fi
	echo "fixture cleanup: status=$status dir_present=$([ -e "$dir" ] && echo yes || echo no)"
	exit $status
fi
dir=${1:?usage: signing-keychain-fixture.sh DIR}
umask 077
mkdir -p "$dir"
cd "$dir"
cat > fixture.cnf <<'CNF'
[req]
distinguished_name = dn
prompt = no
x509_extensions = ext
[dn]
CN = a2a signing keychain control fixture
[ext]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
CNF
/usr/bin/openssl rand -hex 16 > identity.pass
/usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 1 -config fixture.cnf -keyout key.pem -out cert.pem 2>/dev/null
/usr/bin/openssl pkcs12 -export -inkey key.pem -in cert.pem -name fixture -passout file:identity.pass -out identity.p12
rm -f key.pem
echo "fixture: $(/usr/bin/openssl x509 -in cert.pem -noout -subject -enddate | tr '\n' ' ')"
echo "fixture: p12 sha256 $(/usr/bin/shasum -a 256 identity.p12 | cut -d' ' -f1) size $(wc -c < identity.p12 | tr -d ' ')"
