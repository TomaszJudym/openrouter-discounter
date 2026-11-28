#!/bin/sh
# Local stand-in for the GitHub Actions presets reruns: repoint the OpenRouter
# presets and the local pi agent at today's best deals. Schedule it every 2 h
# with cron, launchd (deploy/com.openrouter-discounter.plist) or a systemd
# user timer (deploy/openrouter-discounter.service + .timer).
#
# All output goes to stderr: a scheduler's stdout may be a report channel, and
# a half-line there is worse than none. Secrets come from the environment
# only (OPENROUTER_MANAGEMENT_KEY, AA_API_KEY, DARKBLOOM_API_KEY); nothing is
# printed, written or committed. TRACKER_REPORT=false makes the tracker run
# presets-only: no Telegram secrets and no message.
#
# Usage: scripts/local-cron.sh [--dry-run]
#   --dry-run is passed to pipresets: it prints what would change and writes
#   nothing. The tracker's preset updates are not dry-run.
set -eu

unset CDPATH
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

export TRACKER_REPORT=false

log() { echo "local-cron: $*" >&2; }

log "tracker presets: start"
go run ./cmd/tracker >&2

log "pipresets: start"
go run ./cmd/pipresets "$@" >&2

log "done"
