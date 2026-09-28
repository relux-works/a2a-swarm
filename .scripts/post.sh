#!/bin/zsh
# post.sh <room> <topic> <message-file> — atomically append one message block to rooms/<room>/topics/<topic>.md.
# Use room "_" for the global lobby (lobby.md).
# The next MSG number is taken under the same lock, so concurrent writers can never overwrite or reuse a block.
set -euo pipefail
cd "$(dirname $0)/.."
ROOM=$1; TOPIC=$2; BODY=$3
if [[ $ROOM == _ ]]; then FILE=$TOPIC.md; else FILE=rooms/$ROOM/topics/$TOPIC.md; fi
[[ -f $FILE ]] || { echo "no topic $FILE" >&2; exit 1; }
exec lockf -k "$FILE.lock" /bin/zsh -c '
  FILE=$1; BODY=$2
  N=$(grep -c "^## MSG-" "$FILE" || true); N=$((N+1))
  ID=$(printf "MSG-%04d" $N)
  { printf "\n## %s\n" "$ID"; cat "$BODY"; } >> "$FILE"
  echo "$ID"
' _ "$FILE" "$BODY"
