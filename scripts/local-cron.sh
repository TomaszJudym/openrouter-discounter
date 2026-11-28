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
# The tracker is the single writer: it repoints the presets and, when
# TRACKER_PIPRESETS names the agent dir, emits models.json/settings.json from
# the same pipeline run. No pipresets call is needed on the ticking path, so
# there is one fetch and one write of the pi config per run.
#
# Usage: scripts/local-cron.sh [--dry-run]
#   --dry-run writes no pi config: the tracker runs with TRACKER_PIPRESETS
#   unset (its emit is opt-in), and pipresets --dry-run prints the changes it
#   would make. Preset repointing is not a dry run, here or in the tracker.
set -eu

unset CDPATH
root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

export TRACKER_REPORT=false

dry_run=0
case "${1:-}" in
	"") ;;
	--dry-run) dry_run=1 ;;
	*)
		echo "usage: $0 [--dry-run]" >&2
		exit 2
		;;
esac

pi_dir=${TRACKER_PIPRESETS:-$HOME/.pi/agent}

log() { echo "local-cron: $*" >&2; }

log "tracker presets: start"
if [ "$dry_run" = 1 ]; then
	unset TRACKER_PIPRESETS
	go run ./cmd/tracker >&2
	log "pipresets dry run: what would change"
	go run ./cmd/pipresets --dry-run --dir "$pi_dir" >&2
else
	export TRACKER_PIPRESETS="$pi_dir"
	log "tracker writes the pi config to $TRACKER_PIPRESETS"
	go run ./cmd/tracker >&2
fi

log "done"
