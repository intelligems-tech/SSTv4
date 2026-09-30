#!/usr/bin/env bash
# End-to-end smoke for the pooled dev worker. Needs AWS credentials, the fork
# built at $SST_BIN_PATH (or a `sst` on PATH), `aws` and `jq`.
#
#   SST_BIN_PATH=/tmp/sst-ig ./smoke.sh
#
# It runs `sst dev` on a throwaway stage, invokes the three fixture functions
# concurrently, checks that every response carries its own HANDLER_FN and the
# RESOURCE_ENV override from this process, that no more than
# SST_WORKER_POOL_SIZE workers were started, that touching the bundle's
# .last-rebuild retires and replaces workers, and removes the stage.
set -euo pipefail

cd "$(dirname "$0")"
SST="${SST_BIN_PATH:-sst}"
STAGE="${SMOKE_STAGE:-smoke-$USER}"
POOL_SIZE="${SST_WORKER_POOL_SIZE:-2}"
export SST_WORKER_POOL_SIZE="$POOL_SIZE"
export SST_DEV_ENV_OVERRIDES="RESOURCE_ENV"
export RESOURCE_ENV="from-dev-process"
export SST_PRINT_LOGS=true

LOG=".sst/log/sst.log"
rm -f "$LOG"
touch .bundle/.last-rebuild

"$SST" dev --stage "$STAGE" --mode=basic >smoke-dev.out 2>&1 &
DEV_PID=$!
cleanup() {
  kill "$DEV_PID" 2>/dev/null || true
  wait "$DEV_PID" 2>/dev/null || true
  "$SST" remove --stage "$STAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "waiting for the dev deploy"
for _ in $(seq 1 120); do
  if [ -f ".sst/$STAGE.server" ]; then
    SERVER="http://$(cat ".sst/$STAGE.server")"
    if curl -fs "$SERVER/api/completed" 2>/dev/null | jq -e '.finished == true' >/dev/null 2>&1; then
      break
    fi
  fi
  sleep 5
done
curl -fs "$SERVER/api/completed" | jq -e '.finished == true' >/dev/null || {
  echo "dev deploy did not finish"; tail -50 smoke-dev.out; exit 1;
}

names() {
  jq -r ".$1" .sst/outputs.json
}

invoke_all() {
  local round="$1"
  local pids=()
  for id in a b c; do
    for n in $(seq 1 10); do
      aws lambda invoke --function-name "$(names "$id")" \
        --payload "{\"round\":\"$round\",\"n\":$n}" --cli-binary-format raw-in-base64-out \
        "out-$round-$id-$n.json" >/dev/null &
      pids+=($!)
    done
  done
  for pid in "${pids[@]}"; do wait "$pid"; done
  for id in a b c; do
    for n in $(seq 1 10); do
      jq -e --arg id "$id" '.handlerFn == $id and .resourceEnv == "from-dev-process"' "out-$round-$id-$n.json" >/dev/null || {
        echo "response $round/$id/$n did not carry its own env:"; cat "out-$round-$id-$n.json"; exit 1;
      }
    done
  done
}

echo "round 1: 30 concurrent invocations across 3 functions"
invoke_all one
STARTED=$(grep -c "pooled worker started" "$LOG" || true)
echo "workers started: $STARTED (pool size $POOL_SIZE)"
[ "$STARTED" -le "$POOL_SIZE" ] || { echo "more workers than the pool size"; exit 1; }
[ "$STARTED" -ge 1 ] || { echo "no pooled worker started"; exit 1; }
DISTINCT_PIDS=$(jq -s '[.[].pid] | unique | length' out-one-*.json)
[ "$DISTINCT_PIDS" -le "$POOL_SIZE" ] || { echo "responses came from $DISTINCT_PIDS processes"; exit 1; }

echo "round 2: rebuild stamp retires workers"
sleep 1
touch .bundle/.last-rebuild
sleep 4
invoke_all two
STOPPED=$(grep -c "pooled worker stopped" "$LOG" || true)
STARTED2=$(grep -c "pooled worker started" "$LOG" || true)
[ "$STOPPED" -ge 1 ] || { echo "no worker was retired after the rebuild"; exit 1; }
[ "$STARTED2" -gt "$STARTED" ] || { echo "no fresh worker after the rebuild"; exit 1; }

echo "smoke passed: started=$STARTED2 stopped=$STOPPED"
rm -f out-*.json
