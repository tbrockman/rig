#!/usr/bin/env bash
# rig-agent runner. Installed into the guest by `rig agent start`; not meant to
# be edited in place, because rig overwrites it on every start.
#
# Everything here exists to make an unattended agent survivable:
#
#   - The session UUID is fixed by rig and stored, so a crash resumes the
#     conversation rather than restarting the brief from the top. Without this,
#     a restarted agent rebuilds its state by reading git log and its own
#     journal, which works only if it has been committing diligently.
#   - Operator messages are a file, not a pipe. A file survives a crash, needs
#     no reader on the other end, and cannot couple the two processes'
#     lifetimes. It is injected at the next (re)launch.
#   - Output is stream-json to a file. It never reaches the operator's context
#     unless they ask for it, and it means progress is visible while the agent
#     is still working rather than only when it finishes.
set -u

# Both overridable so the script can be exercised outside a guest. rig always
# uses the defaults; nothing sets these in production.
D="${RIG_AGENT_DIR:-/var/lib/rig-agent}"
WORKDIR="${RIG_AGENT_WORKDIR:?}"
SID="${RIG_AGENT_SESSION:?}"
TIMEOUT="${RIG_AGENT_TIMEOUT:-6h}"

mkdir -p "$D"
cd "$WORKDIR" || exit 78   # EX_CONFIG: nothing to resume into

# --- credentials ---------------------------------------------------------
# Same contract as the rest of rig: the secret lives on tmpfs and dies with the
# VM. Only the transcript is allowed on disk.
ENV_FILE="${RIG_AGENT_ENV:-/run/rig/env}"
if [ ! -r "$ENV_FILE" ]; then
  echo "no credentials at $ENV_FILE" >&2
  exit 78
fi
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

export CLAUDE_CONFIG_DIR="${RIG_AGENT_CONFIG_DIR:-/run/rig/claude}"
install -d -m 700 "$CLAUDE_CONFIG_DIR"
# Three ways to authenticate, ordered by how well they survive an unattended
# run. Whichever the operator supplied wins.
#
#   CLAUDE_CODE_OAUTH_TOKEN  long-lived, from `claude setup-token`. Preferred:
#                            it does not expire in hours and nothing else
#                            rotates it behind our back.
#   ANTHROPIC_API_KEY        an API key. Same property.
#   CLAUDE_CREDENTIALS_B64   a snapshot of ~/.claude/.credentials.json. Works,
#                            but is FRAGILE: refreshing an OAuth session
#                            ROTATES its refresh token, so any other consumer of
#                            that credential — a Claude Code session on the host,
#                            say — kills this copy the moment it refreshes. It
#                            fails as "OAuth session expired and could not be
#                            refreshed", and claude then rewrites the credential
#                            file with both tokens blanked, so retrying cannot
#                            help.
if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ] \
   && [ -z "${CLAUDE_CREDENTIALS_B64:-}" ]; then
  echo "no credentials in $ENV_FILE. Define one of CLAUDE_CODE_OAUTH_TOKEN" >&2
  echo "(preferred), ANTHROPIC_API_KEY, or CLAUDE_CREDENTIALS_B64." >&2
  exit 78
fi

# Materialise the snapshot only when it is the only credential on offer: a token
# in the environment is what claude prefers anyway, and a stale credential file
# sitting beside one invites the refresh failure described above.
if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  printf '%s' "$CLAUDE_CREDENTIALS_B64" | base64 -d > "$CLAUDE_CONFIG_DIR/.credentials.json"
  chmod 600 "$CLAUDE_CONFIG_DIR/.credentials.json"
fi
unset CLAUDE_CREDENTIALS_B64

# Transcripts are work product, not secrets. They belong on disk, or --resume
# has nothing to resume from after a VM stop. A directory symlink is safe where
# a file symlink would not be: claude appends to files inside projects/, it
# never replaces the directory.
install -d -m 700 "$D/projects"
if [ ! -L "$CLAUDE_CONFIG_DIR/projects" ]; then
  rm -rf "$CLAUDE_CONFIG_DIR/projects"
  ln -s "$D/projects" "$CLAUDE_CONFIG_DIR/projects"
fi

# claude refuses --dangerously-skip-permissions as root, and every rig guest is
# root. The containment is the VM boundary and the network ACL, both of which
# `rig verify` proves from inside the guest.
export IS_SANDBOX=1

# --- prompt: the brief, plus anything the operator queued -----------------
PROMPT="$(cat "$D/prompt" 2>/dev/null || true)"

if [ -s "$D/inbox" ]; then
  PROMPT="$PROMPT

--- OPERATOR MESSAGE (queued since your last turn; act on it now) ---
$(cat "$D/inbox")
--- END OPERATOR MESSAGE ---"
  # Keep what was delivered, so a crash between here and claude starting does
  # not silently eat the message.
  cat "$D/inbox" >> "$D/inbox.delivered"
  : > "$D/inbox"
fi

# --- resume or start ------------------------------------------------------
# Decide from evidence, not from a marker we set ourselves. An earlier version
# touched a "started" file before exec'ing claude, so a launch that failed for
# any reason — the binary not on PATH, say — left the state claiming a session
# existed. Every later run then asked to --resume a conversation that was never
# created and died with "No conversation found with session ID", which is a
# self-inflicted wound that no amount of restarting can heal.
#
# The transcript is the evidence: claude writes one per session, and we keep
# projects/ on disk precisely so it survives. If it is there, resume; if not,
# start. That is self-correcting — a failed first run leaves nothing behind and
# the next attempt simply starts cleanly.
if compgen -G "$D/projects/*/$SID.jsonl" > /dev/null; then
  MODE=(--resume "$SID")
  RESTARTS=$(( $(cat "$D/restarts" 2>/dev/null || echo 0) + 1 ))
  echo "$RESTARTS" > "$D/restarts"
  PROMPT="$PROMPT

--- NOTE: this process was restarted (restart #$RESTARTS) ---
The previous process exited unexpectedly. Your conversation is resumed, but any
tool call in flight at that moment did not finish. Before doing anything else,
check the working tree state (git status, git log) and reconcile it with what
you believe you had done."
else
  MODE=(--session-id "$SID")
  : > "$D/restarts"
fi

timeout "$TIMEOUT" claude -p "$PROMPT" "${MODE[@]}" \
  --dangerously-skip-permissions \
  --verbose \
  --output-format stream-json \
  >> "$D/events.jsonl" 2>> "$D/stderr.log"

RC=$?
echo "$RC" > "$D/last_exit"

# 0 means the agent decided it was finished. Anything else is a failure that
# systemd should retry — exit non-zero so Restart=on-failure sees it.
exit "$RC"
