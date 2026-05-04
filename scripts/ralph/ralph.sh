#!/bin/bash
# Ralph Wiggum - Long-running AI agent loop
# Usage: ./ralph.sh [--tool amp|claude] [max_iterations]

set -e

# Parse arguments
TOOL="amp"  # Default to amp for backwards compatibility
MAX_ITERATIONS=10

while [[ $# -gt 0 ]]; do
  case $1 in
    --tool)
      TOOL="$2"
      shift 2
      ;;
    --tool=*)
      TOOL="${1#*=}"
      shift
      ;;
    *)
      # Assume it's max_iterations if it's a number
      if [[ "$1" =~ ^[0-9]+$ ]]; then
        MAX_ITERATIONS="$1"
      fi
      shift
      ;;
  esac
done

# Validate tool choice
if [[ "$TOOL" != "amp" && "$TOOL" != "claude" ]]; then
  echo "Error: Invalid tool '$TOOL'. Must be 'amp' or 'claude'."
  exit 1
fi
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PRD_FILE="$SCRIPT_DIR/prd.json"
PROGRESS_FILE="$SCRIPT_DIR/progress.txt"
ARCHIVE_DIR="$SCRIPT_DIR/archive"
LAST_BRANCH_FILE="$SCRIPT_DIR/.last-branch"

# Archive helper: snapshot prd.json + progress.txt under a folder named after
# the cycle's branchName (read from prd.json itself — folder name always
# matches content). Stamped with the current date.
archive_cycle() {
  local prd="$1"
  local progress="$2"
  local label="$3"  # human-readable trigger ("complete", "abandoned", etc.)

  if [ ! -f "$prd" ]; then
    return 0
  fi

  local branch
  branch=$(jq -r '.branchName // empty' "$prd" 2>/dev/null || echo "")
  if [ -z "$branch" ]; then
    return 0
  fi

  local date_stamp folder_name archive_folder
  date_stamp=$(date +%Y-%m-%d)
  folder_name=$(echo "$branch" | sed 's|^ralph/||')
  archive_folder="$ARCHIVE_DIR/$date_stamp-$folder_name"

  # If the folder already exists from an earlier same-day archive, suffix.
  if [ -d "$archive_folder" ]; then
    archive_folder="$archive_folder-$label"
  fi

  echo "Archiving cycle ($label): $branch -> $archive_folder"
  mkdir -p "$archive_folder"
  cp "$prd" "$archive_folder/"
  [ -f "$progress" ] && cp "$progress" "$archive_folder/"
}

# Detect a branch transition that bypassed cycle-end archiving. The user's
# /ralph-skills:ralph flow overwrites prd.json directly — when that happens
# the previous cycle's prd.json content is already gone, but we still log
# that the previous branch was abandoned mid-flight so future audits can
# see the gap. We do NOT try to archive prd.json under the old branch name
# (that mislabels the new cycle's content as the old one's, which is the
# bug this rewrite fixes).
if [ -f "$PRD_FILE" ] && [ -f "$LAST_BRANCH_FILE" ]; then
  CURRENT_BRANCH=$(jq -r '.branchName // empty' "$PRD_FILE" 2>/dev/null || echo "")
  LAST_BRANCH=$(cat "$LAST_BRANCH_FILE" 2>/dev/null || echo "")

  if [ -n "$CURRENT_BRANCH" ] && [ -n "$LAST_BRANCH" ] && [ "$CURRENT_BRANCH" != "$LAST_BRANCH" ]; then
    echo "WARN: branch transition $LAST_BRANCH -> $CURRENT_BRANCH detected without prior cycle-end archive."
    echo "      If $LAST_BRANCH cycle ended, its prd.json was already overwritten and its artifacts"
    echo "      are not recoverable from this script. Future cycles should archive on completion"
    echo "      via the COMPLETE handler below or via the prep-cycle skill."
    # Reset progress file so the new cycle starts clean.
    echo "# Ralph Progress Log" > "$PROGRESS_FILE"
    echo "Started: $(date)" >> "$PROGRESS_FILE"
    echo "Cycle: $CURRENT_BRANCH" >> "$PROGRESS_FILE"
    echo "---" >> "$PROGRESS_FILE"
  fi
fi

# Track current branch for transition detection on next run.
if [ -f "$PRD_FILE" ]; then
  CURRENT_BRANCH=$(jq -r '.branchName // empty' "$PRD_FILE" 2>/dev/null || echo "")
  if [ -n "$CURRENT_BRANCH" ]; then
    echo "$CURRENT_BRANCH" > "$LAST_BRANCH_FILE"
  fi
fi

# Initialize progress file if it doesn't exist
if [ ! -f "$PROGRESS_FILE" ]; then
  echo "# Ralph Progress Log" > "$PROGRESS_FILE"
  echo "Started: $(date)" >> "$PROGRESS_FILE"
  echo "---" >> "$PROGRESS_FILE"
fi

echo "Starting Ralph - Tool: $TOOL - Max iterations: $MAX_ITERATIONS"

for i in $(seq 1 $MAX_ITERATIONS); do
  echo ""
  echo "==============================================================="
  echo "  Ralph Iteration $i of $MAX_ITERATIONS ($TOOL)"
  echo "==============================================================="

  # Run the selected tool with the ralph prompt
  if [[ "$TOOL" == "amp" ]]; then
    OUTPUT=$(cat "$SCRIPT_DIR/prompt.md" | amp --dangerously-allow-all 2>&1 | tee /dev/stderr) || true
  else
    # Claude Code via avito ai wrapper (corporate auth). --dangerously-skip-permissions
    # for autonomous operation, --print for non-interactive output. The `--` separator
    # forwards subsequent flags to the underlying claude CLI unchanged.
    # NOTE: This wrapper is mandatory for this repo — direct `claude` invocation does
    # not authenticate against the corporate proxy. Do not revert.
    OUTPUT=$(avito ai claude -- --dangerously-skip-permissions --print < "$SCRIPT_DIR/CLAUDE.md" 2>&1 | tee /dev/stderr) || true
  fi
  
  # Check for completion signal
  if echo "$OUTPUT" | grep -q "<promise>COMPLETE</promise>"; then
    echo ""
    echo "Ralph completed all tasks!"
    echo "Completed at iteration $i of $MAX_ITERATIONS"
    # Archive the just-completed cycle's artifacts. Folder name is derived
    # from prd.json's own branchName so it always matches content.
    archive_cycle "$PRD_FILE" "$PROGRESS_FILE" "complete"
    exit 0
  fi
  
  echo "Iteration $i complete. Continuing..."
  sleep 2
done

echo ""
echo "Ralph reached max iterations ($MAX_ITERATIONS) without completing all tasks."
echo "Check $PROGRESS_FILE for status."
exit 1
