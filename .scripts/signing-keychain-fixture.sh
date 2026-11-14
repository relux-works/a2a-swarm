#!/bin/bash
# Creates a throwaway self-signed code-signing identity for the native
# signing-keychain control and trusts it for code signing on this runner only.
# It is never a release credential. Usage: signing-keychain-fixture.sh DIR
set -euo pipefail
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
/usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 1 -config fixture.cnf -keyout key.pem -out cert.pem
/usr/bin/openssl pkcs12 -export -inkey key.pem -in cert.pem -name fixture -passout file:identity.pass -out identity.p12
rm -f key.pem
sudo /usr/bin/security add-trusted-cert -d -r trustRoot -p codeSign -k /Library/Keychains/System.keychain cert.pem
/usr/bin/openssl version
echo "fixture: $(/usr/bin/openssl x509 -in cert.pem -noout -subject -enddate | tr '\n' ' ')"
