#!/usr/bin/env bash
set -euo pipefail

if [ -z "${SLACK_WEBHOOK_URL:-}" ]; then
  echo "SLACK_WEBHOOK_URL not set, skipping notification"
  exit 0
fi

REPO="${CI_REPO:-unknown}"
PIPELINE="${CI_PIPELINE_NUMBER:-0}"
STATUS="${CI_PIPELINE_STATUS:-unknown}"
BRANCH="${CI_COMMIT_BRANCH:-unknown}"
COMMIT_MSG=$(echo "${CI_COMMIT_MESSAGE:-}" | head -1 | cut -c1-80)
LINK="${CI_PIPELINE_FORGE_URL:-}"

if [ "$STATUS" = "success" ]; then
  COLOR="#36a64f"
  EMOJI=":white_check_mark:"
else
  COLOR="#dc3545"
  EMOJI=":x:"
fi

curl -s -X POST "$SLACK_WEBHOOK_URL" \
  -H "Content-Type: application/json" \
  -d "{
    \"attachments\": [{
      \"color\": \"${COLOR}\",
      \"text\": \"${EMOJI} *${REPO}* #${PIPELINE} ${STATUS}\\n${BRANCH}: ${COMMIT_MSG}\\n${LINK}\"
    }]
  }" || echo "Slack notification failed (non-fatal)"
