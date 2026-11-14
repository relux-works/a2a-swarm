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
# The bound is checked on this runner before it is relied on: a known hang
# must end with 142 inside its bound, and a quick command must return 0.
bound_control() {
	local hang=0 quick=0 started=$SECONDS
	bounded 1 /bin/sleep 5 || hang=$?
	bounded 5 /usr/bin/true || quick=$?
	echo "bound control: hang=$hang quick=$quick elapsed=$((SECONDS - started))s"
	[ "$hang" -eq 142 ] && [ "$quick" -eq 0 ] && [ $((SECONDS - started)) -lt 4 ]
}
bound_control
if [ "${1:-}" = "--cleanup" ]; then
	dir=${2:?usage: signing-keychain-fixture.sh --cleanup DIR}
	status=0
	rm -rf "$dir" || status=1
	if [ -e "$dir" ]; then status=1; fi
	# Fail closed: an unreadable or timed-out search list is never "clean".
	list_rc=0
	listing=$(bounded 30 /usr/bin/security list-keychains -d user) || list_rc=$?
	if [ "$list_rc" -ne 0 ]; then
		status=1 search="unknown:$list_rc"
	elif printf '%s\n' "$listing" | grep -q 'control.keychain-db'; then
		status=1 search=fixture-present
	else
		search=clean
	fi
	echo "fixture cleanup: status=$status dir_present=$([ -e "$dir" ] && echo yes || echo no) search_list=$search"
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
