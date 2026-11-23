#!/bin/bash
# The release keeper, beside the gate in the gate's pod. It keeps the proxy on the latest
# build of the fork (github.com/Mach1212/cliproxyapi-fork), and neither cuts nor refuses a
# request to do it.
#
# Every hour it reads the fork's latest release, which names its image in a line
# "image: <repository>:<tag>@sha256:<digest>". When that differs from what runs and has not
# failed here before, it watches the gate through the HAProxy runtime API for a moment when
# no client connection is open. Nobody waits for that moment: the gate stays open, and under
# steady load the build waits with the keeper. At that moment the keeper holds the gate (it
# takes no new connection, so a request that arrives waits in the port's queue), patches
# only the proxy's image and the cliproxy/* annotations, waits until the new proxy answers,
# and opens the gate: the requests that waited go to the new build. It then checks that
# build for 10 minutes.
#
# A build that does not answer within KEEPER_START seconds is taken out in the same hold: the
# keeper goes back to the previous image. A build that fails the check later is taken out
# with a hold as well: new requests wait while the open answers finish, KEEPER_HOLD seconds
# at most, and when answers still run by then the gate opens and the keeper tries again five
# minutes later. Either way that release is skipped.
#
# The fork publishes a build only when it stands (its hooks fit upstream's release and its
# checks pass), so a release of upstream that the fork cannot follow never gets here.
#
# State: the Deployment's cliproxy/{release,image,previous,upgraded-at,verify,skip}
# annotations, and a marker of the change in hand in an emptyDir, so a keeper that restarts
# in the middle of one takes it up and opens the gate.
set -uo pipefail

NAMESPACE=${NAMESPACE:?}
DEPLOY=${KEEPER_DEPLOYMENT:?}
CONTAINER=${KEEPER_CONTAINER:-cliproxyapi}
# The image repository the fork publishes to, such as ghcr.io/mach1212/cliproxyapi-fork.
REPO=${KEEPER_REPOSITORY:?}
RELEASE_URL=${KEEPER_RELEASE_URL:?}
INTERVAL=${KEEPER_INTERVAL:-3600}
RETRY_WAIT=${KEEPER_RETRY_WAIT:-300}
HEALTH_WINDOW=${KEEPER_HEALTH_WINDOW:-600}
HEALTH_GRACE=${KEEPER_HEALTH_GRACE:-300}
# How long a build has to answer once its image is set, and how long new requests may wait
# for the open answers when a failing build is taken out.
START=${KEEPER_START:-60}
HOLD=${KEEPER_HOLD:-90}
# Without a key, the proxy answers 401. Any other answer (503 from the gate, or 200 meaning
# nobody is asked for a key) is unhealthy. HEALTH_URL goes through the gate as a client's
# request does. PROXY_URL goes through the gate's loopback frontend, which a hold leaves
# open.
HEALTH_URL=${KEEPER_HEALTH_URL:?}
PROXY_URL=${KEEPER_PROXY_URL:?}
STATE=${KEEPER_STATE:-/run/keeper}
MARKER=$STATE/change
TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+-pool\.[0-9]+$'
# A build of the fork: <repository>:<tag>@sha256:<digest>.
IMAGE_RE="^${REPO//./\\.}:v[0-9]+\\.[0-9]+\\.[0-9]+-pool\\.[0-9]+@sha256:[0-9a-f]{64}\$"
# What a rollback may go back to: a build of the fork, or the image of upstream the
# deployment ran before it followed the fork.
PREVIOUS_RE="$IMAGE_RE|^eceasy/cli-proxy-api:v[0-9]+\\.[0-9]+\\.[0-9]+@sha256:[0-9a-f]{64}\$"
NEXT=$INTERVAL

log() { printf '%s keeper: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
nap() { sleep "$1" & wait $!; }
trap 'log "stopping"; exit 0' TERM INT

# HAProxy runtime API (127.0.0.1:9999 in this pod).
rt() {
  local rc
  { exec 3<>/dev/tcp/127.0.0.1/9999; } 2>/dev/null || return 1
  printf '%s\n' "$1" >&3
  timeout 10 cat <&3
  rc=$?
  exec 3<&-
  return $rc
}
# How many client connections the gate's frontend has, and how many it takes at most.
conns() { rt 'show stat' | awk -F, '$1 == "api" && $2 == "FRONTEND" { print $5 }'; }
limit() { rt 'show stat' | awk -F, '$1 == "api" && $2 == "FRONTEND" { print $7 }'; }
# Hold: the gate takes no new connection, and closes each open one once it is idle.
hold() {
  rt 'experimental-mode on; set var proc.holding int(1)' >/dev/null
  rt 'set maxconn frontend api 0' >/dev/null
  [ "$(limit)" = 0 ]
}
# Open: back to the frontend's own limit, the process's. The waiting connections come first.
open_gate() {
  local max
  max=$(rt 'show info' | awk -F': *' '$1 == "Maxconn" { print $2 }')
  [[ $max =~ ^[1-9][0-9]*$ ]] || return 1
  rt "set maxconn frontend api $max" >/dev/null
  rt 'experimental-mode on; set var proc.holding int(0)' >/dev/null
  [ "$(limit)" = "$max" ]
}
# A gate left held answers nobody, so this does not give up. The gate starts beside the
# keeper, so the first tries may come before its runtime API is there.
reopen() {
  local n=0
  until open_gate; do
    if ((n++ % 15 == 2)); then log "the gate does not open: its runtime API does not answer; trying on"; fi
    nap 2
  done
}

# A call that hangs while the gate is held would keep every client waiting.
# Wrapped with timeout rather than --request-timeout, which disables kubectl's in-cluster config.
kube() { timeout 15 kubectl -n "$NAMESPACE" "$@"; }
deploy_json() { kube get deployment "$DEPLOY" -o json; }
image_of() { jq -r --arg c "$CONTAINER" '.spec.template.spec.containers[] | select(.name == $c) | .image' <<<"$1"; }
ann_of() { jq -r --arg k "cliproxy/$2" '.metadata.annotations[$k] // ""' <<<"$1"; }
# The tag of an image the keeper knows: what is between the repository and the digest.
tag_of() {
  [[ $1 =~ $PREVIOUS_RE ]] || return 1
  local tag=${1%@*}
  printf '%s' "${tag##*:}"
}
annotate() { kube annotate --overwrite deployment "$DEPLOY" "$@" >/dev/null; }
set_image() {
  kube patch deployment "$DEPLOY" --type strategic -p "$(
    jq -nc --arg c "$CONTAINER" --arg i "$1" --argjson a "$2" \
      '{metadata: {annotations: $a}, spec: {template: {spec: {containers: [{name: $c, image: $i}]}}}}'
  )" >/dev/null
}
# stamp MODE TARGET FROM: the annotations that go with the patch to TARGET.
stamp() {
  jq -nc --arg m "$1" --arg r "$(tag_of "$2")" --arg i "$2" --arg p "$3" \
    --arg t "$(date +%s)" --arg s "$(tag_of "$3")" '
    {"cliproxy/release": $r, "cliproxy/image": $i, "cliproxy/previous": $p, "cliproxy/upgraded-at": $t}
    + if $m == "rollback" then {"cliproxy/verify": "rolled-back", "cliproxy/skip": $s}
      else {"cliproxy/verify": "pending"} end'
}

# quiet: watch for a moment when no client connection is open, until the next look at the
# releases, and hold the gate at it. Nobody waits for that moment.
quiet() {
  local end
  end=$(($(date +%s) + INTERVAL))
  while (($(date +%s) < end)); do
    if [ "$(conns)" = 0 ]; then
      # A connection that got in before the hold took is an answer under way: not the moment.
      if hold && [ "$(conns)" = 0 ]; then return 0; fi
      reopen
    fi
    nap 1
  done
  return 1
}

# drain: hold the gate and wait until no client connection is open, HOLD seconds at most.
# New requests wait meanwhile.
drain() {
  local end c
  end=$(($(date +%s) + HOLD))
  while :; do
    hold || log "rollback: the gate does not take the hold; trying again"
    c=$(conns)
    [ "$c" = 0 ] && [ "$(limit)" = 0 ] && return 0
    if (($(date +%s) >= end)); then
      log "rollback: ${c:-?} client connection(s) still open after ${HOLD}s"
      return 1
    fi
    nap 1
  done
}

# await TARGET: wait until the Deployment runs one pod, of TARGET, and the proxy in it
# answers, START seconds at most. With Recreate the old pod is gone before the new one is made.
await() {
  local target=$1 end d
  end=$(($(date +%s) + START))
  while (($(date +%s) < end)); do
    if d=$(deploy_json) && [ "$(image_of "$d")" = "$target" ] &&
      jq -e '(.status.observedGeneration // 0) >= .metadata.generation
        and (.status.updatedReplicas // 0) == 1 and (.status.replicas // 0) == 1' <<<"$d" >/dev/null &&
      [ "$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$PROXY_URL")" = 401 ]; then
      return 0
    fi
    nap 1
  done
  return 1
}

# land MODE TARGET FROM: after the patch, wait until TARGET answers. An upgrade that does not
# goes back to FROM at once, the gate still held. Returns 0 when TARGET answers.
land() {
  local mode=$1 target=$2 from=$3
  if await "$target"; then
    log "$mode: $target answers"
    return 0
  fi
  log "$mode: $target does not answer after ${START}s"
  [ "$mode" = upgrade ] || return 1
  if ! [[ $from =~ $PREVIOUS_RE ]]; then
    log "upgrade: there is no previous image to roll back to"
    annotate cliproxy/verify=failed
  elif ! set_image "$from" "$(stamp rollback "$from" "$target")"; then
    log "rollback: patch failed"
  elif await "$from"; then
    log "rollback: $from answers again"
  else
    log "rollback: $from does not answer after ${START}s either"
  fi
  return 1
}

# switch_to MODE TARGET FROM: get the gate held with no client connection open, patch the
# image, wait for the new proxy, open the gate. Returns 0 when TARGET runs, 1 when the gate
# did not get there, 2 when TARGET did not answer, and 3 when the patch failed. After 1 and 3
# nothing is changed.
switch_to() {
  local mode=$1 target=$2 from=$3 held=no rc=0
  printf '%s %s %s\n' "$mode" "$target" "$from" >"$MARKER"
  if [ "$mode" = upgrade ]; then
    log "upgrade: $from -> $target; waiting for a moment when no client connection is open ($(conns) now)"
    if quiet; then held=yes; fi
  else
    log "rollback: $from -> $target; new requests wait at the gate while the open ones finish"
    if drain; then held=yes; fi
  fi
  if [ "$held" = no ]; then
    reopen
    rm -f "$MARKER"
    if [ "$mode" = rollback ]; then log "rollback: nothing is changed; the gate is open again"; fi
    return 1
  fi
  log "$mode: no client connection is open and the gate is held; patching the image"
  if ! set_image "$target" "$(stamp "$mode" "$target" "$from")"; then
    reopen
    rm -f "$MARKER"
    log "$mode: patch failed; nothing is changed, the gate is open again"
    return 3
  fi
  land "$mode" "$target" "$from" || rc=2
  reopen
  rm -f "$MARKER"
  log "$mode: the gate is open again"
  return $rc
}

# A keeper that restarts in the middle of a change takes it up where the patch left it, and
# opens the gate.
resume() {
  local mode target from d
  if [ -s "$MARKER" ]; then
    read -r mode target from <"$MARKER"
    if d=$(deploy_json) && [ "$(image_of "$d")" = "$target" ]; then
      log "resume: restarted after the $mode patch"
      land "$mode" "$target" "$from"
    else
      log "resume: restarted before the $mode patch; nothing is changed"
    fi
    rm -f "$MARKER"
  fi
  reopen
}

healthy() {
  local start=$1 end grace ok=0 fails=0 code
  end=$((start + HEALTH_WINDOW))
  grace=$((start + HEALTH_GRACE))
  if (($(date +%s) >= end)); then
    end=$(($(date +%s) + 60))
    grace=$end
  fi
  while (($(date +%s) < end)); do
    code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$HEALTH_URL")
    if [ "$code" = 401 ]; then
      ok=1
      fails=0
    elif ((ok)); then
      if ((++fails >= 3)); then
        log "verify: $fails failed checks in a row (last HTTP $code)"
        return 1
      fi
    elif (($(date +%s) >= grace)); then
      log "verify: no healthy answer within ${HEALTH_GRACE}s (last HTTP $code)"
      return 1
    fi
    nap 10
  done
  ((ok))
}

verify_pending() {
  local d img target at prev rc=0
  until d=$(deploy_json); do
    log "verify: cannot read the deployment; retrying"
    nap 30
  done
  [ "$(ann_of "$d" verify)" = pending ] || return 0
  img=$(image_of "$d")
  target=$(ann_of "$d" image)
  at=$(ann_of "$d" upgraded-at)
  prev=$(ann_of "$d" previous)
  if [ "$img" != "$target" ] || ! [[ $at =~ ^[0-9]+$ ]]; then
    log "verify: the running image is not the one the keeper set; not checking"
    annotate cliproxy/verify=skipped
    return 0
  fi
  log "verify: checking $img for ${HEALTH_WINDOW}s"
  if healthy "$at"; then
    log "verify: $img is healthy"
    annotate cliproxy/verify=ok
  elif [[ $prev =~ $PREVIOUS_RE ]]; then
    switch_to rollback "$prev" "$img" || rc=$?
    # Nothing changed: the check stays pending and comes back soon.
    if [ "$rc" = 1 ] || [ "$rc" = 3 ]; then NEXT=$RETRY_WAIT; fi
  else
    log "verify: $img failed and there is no previous image to roll back to"
    annotate cliproxy/verify=failed
  fi
}

check_release() {
  local d img cur skip release latest target rc=0
  if ! d=$(deploy_json); then
    log "cannot read the deployment"
    NEXT=$RETRY_WAIT
    return
  fi
  # A build that is still being checked comes first.
  [ "$(ann_of "$d" verify)" = pending ] && return
  img=$(image_of "$d")
  cur=$(tag_of "$img")
  skip=$(ann_of "$d" skip)
  if ! release=$(curl -fsS -m 30 -H 'Accept: application/vnd.github+json' "$RELEASE_URL"); then
    log "cannot read the latest release"
    return
  fi
  latest=$(jq -r '.tag_name // ""' <<<"$release")
  if ! [[ $latest =~ $TAG_RE ]]; then
    log "unexpected release tag '${latest:0:40}'"
    return
  fi
  [ "$latest" = "$cur" ] && return
  if [ "$latest" = "$skip" ]; then
    log "release $latest failed its check here before; staying on ${cur:-$img}"
    return
  fi
  # The release's own word for its image, digest included.
  target=$(jq -r '.body // ""' <<<"$release" | tr -d '\r' | sed -n 's/^image: *//p' | head -n 1)
  if ! [[ $target =~ $IMAGE_RE ]] || [ "$(tag_of "$target")" != "$latest" ]; then
    log "release $latest names no image of $REPO; checking again in ${RETRY_WAIT}s"
    NEXT=$RETRY_WAIT
    return
  fi
  switch_to upgrade "$target" "$img" || rc=$?
  case $rc in
  0) verify_pending ;;
  # No such moment in the last hour: read the release again, and watch on.
  1) NEXT=1 ;;
  3) NEXT=$RETRY_WAIT ;;
  esac
}

mkdir -p "$STATE"
log "started for $NAMESPACE/$DEPLOY; release check every ${INTERVAL}s"
resume
while :; do
  NEXT=$INTERVAL
  verify_pending
  check_release
  nap "$NEXT"
done
