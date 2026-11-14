#!/bin/bash
# Throwaway self-signed code-signing identity for the native signing-keychain
# control. It is never a release credential and touches no login keychain.
#   signing-keychain-fixture.sh DIR            create identity.p12/identity.pass in DIR and
#                                              trust the leaf for code signing in the admin
#                                              domain of this ephemeral runner only
#   signing-keychain-fixture.sh --cleanup DIR  remove that trust setting and DIR
set -euo pipefail
if [ "${1:-}" = "--cleanup" ]; then
	dir=${2:?usage: signing-keychain-fixture.sh --cleanup DIR}
	status=0
	if [ -f "$dir/cert.pem" ]; then
		sudo /usr/bin/security remove-trusted-cert -d "$dir/cert.pem" || status=1
	fi
	rm -rf "$dir" || status=1
	if [ -e "$dir" ]; then status=1; fi
	if /usr/bin/security list-keychains -d user | grep -q 'control.keychain-db'; then status=1; fi
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
sudo /usr/bin/security add-trusted-cert -d -r trustRoot -p codeSign -k /Library/Keychains/System.keychain cert.pem
echo "fixture: $(/usr/bin/openssl x509 -in cert.pem -noout -subject -enddate | tr '\n' ' ')"
echo "fixture: p12 sha256 $(/usr/bin/shasum -a 256 identity.p12 | cut -d' ' -f1) size $(wc -c < identity.p12 | tr -d ' ')"
