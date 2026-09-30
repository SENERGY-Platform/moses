#!/usr/bin/env bash
# Interleaved history-run profile, in process against worker (SNRGY-4817 spike, variant B).
#   BIN=dir-with-moses-and-runtime.test DOC=environment.json DAYS=30 ROUNDS=3 tools/spike-4817/profile-interleaved.sh
set -u
: "${BIN:?}" "${DOC:?}"
DAYS=${DAYS:-30}
ROUNDS=${ROUNDS:-3}
cd "$(dirname "$0")/../../lib/runtime" || exit 1
for round in $(seq 1 "$ROUNDS"); do
  for test in TestSpikeProfileInProcess TestSpikeProfileWorker; do
    MOSES_WORKER_BINARY="$BIN/moses" MOSES_PROFILE_ENVIRONMENT="$DOC" MOSES_PROFILE_DAYS="$DAYS" \
      "$BIN/runtime.test" -test.run "^${test}\$" -test.timeout 3h 2>/dev/null | grep '^SPIKE' | sed "s/^/round=$round /"
  done
done
