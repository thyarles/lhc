#!/usr/bin/env bash
# Move an issue or pull request on the project board.
#
#   scripts/board.sh 15 "In progress"
#   scripts/board.sh 15 "In review"
#
# Adds the item to the board first if it is not there yet (a new issue), so
# the same command works for every step. Columns: Todo, In progress,
# In review, Done. Needs `gh` with the `project` scope
# (gh auth refresh -h github.com -s project).
set -euo pipefail

OWNER="${BOARD_OWNER:-thyarles}"
PROJECT="${BOARD_NUMBER:-4}"
REPO="${BOARD_REPO:-thyarles/lhc}"

[ $# -eq 2 ] || { echo "usage: $0 NUMBER \"Todo|In progress|In review|Done\"" >&2; exit 2; }
num="$1" status="$2"

# Issue or pull request? Both share one number sequence.
if gh pr view "$num" --repo "$REPO" --json number >/dev/null 2>&1; then
    url="https://github.com/$REPO/pull/$num"
else
    url="https://github.com/$REPO/issues/$num"
fi

project_id=$(gh project view "$PROJECT" --owner "$OWNER" --format json --jq .id)
read -r field_id option_id < <(gh project field-list "$PROJECT" --owner "$OWNER" --format json \
    --jq ".fields[] | select(.name==\"Status\") | \"\(.id) \(.options[] | select(.name==\"$status\") | .id)\"")
[ -n "${option_id:-}" ] || { echo "no column \"$status\" on the board" >&2; exit 1; }

item_id=$(gh project item-list "$PROJECT" --owner "$OWNER" --limit 500 --format json \
    --jq ".items[] | select(.content.number==$num and .content.repository==\"$REPO\") | .id")
if [ -z "$item_id" ]; then
    item_id=$(gh project item-add "$PROJECT" --owner "$OWNER" --url "$url" --format json --jq .id)
fi

gh project item-edit --id "$item_id" --project-id "$project_id" \
    --field-id "$field_id" --single-select-option-id "$option_id" >/dev/null
echo "#$num → $status"
