#!/bin/zsh
# post.sh <room> <topic> <message-file> — atomically append one message block to rooms/<room>/topics/<topic>.md
# (room "_" = the global lobby.md), then commit exactly that file (signed) and push it to origin/main.
# Communication never goes through pull requests (AGENTS.md). The next MSG number is assigned under the same
# lock that serialises the append, the commit and the push, so concurrent posters can neither overwrite nor reuse a block.
set -euo pipefail
cd "$(dirname $0)/.."
ROOM=$1; TOPIC=$2; BODY=$3
if [[ $ROOM == _ ]]; then FILE=$TOPIC.md; else FILE=rooms/$ROOM/topics/$TOPIC.md; fi
[[ -f $FILE ]] || { echo "no topic $FILE" >&2; exit 1; }
exec lockf -k .git/a2a-post.lock /bin/zsh -c '
  set -euo pipefail
  FILE=$1; BODY=$2; ROOM=$3; TOPIC=$4
  N=$(grep -c "^## MSG-" "$FILE" || true); N=$((N+1))
  ID=$(printf "MSG-%04d" $N)
  { printf "\n## %s\n" "$ID"; cat "$BODY"; } >> "$FILE"
  git commit -q -S -m "$ID $ROOM/$TOPIC" -- "$FILE"
  if ! git push -q origin HEAD:main 2>/dev/null; then
    if git pull -q --rebase origin main && git push -q origin HEAD:main; then :; else
      echo "$ID committed locally; push failed (will go out with the next post)" >&2
    fi
  fi
  echo "$ID"
' _ "$FILE" "$BODY" "$ROOM" "$TOPIC"
