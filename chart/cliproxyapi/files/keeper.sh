#!/bin/bash
# The release keeper, a sidecar in the proxy's pod. It keeps the proxy on the latest build of
# the fork (github.com/Mach1212/cliproxyapi-fork), and never cuts a request to do it.
#
# Every hour it reads the fork's latest release, which names its image in a line
# "image: <repository>:<tag>@sha256:<digest>". When that differs from what runs and has not
# failed here before, it drains the gate through the HAProxy runtime API: new connections are
# refused, keep-alive connections close after their current response. It waits, with no time
# limit, until no client connection remains, and patches only the proxy's image and the
# cliproxy/* annotations. The Recreate strategy replaces the pod, and the new gate accepts
# again. The keeper in the new pod checks the new build for 10 minutes. If the check fails,
# it drains the same way, rolls back to the previous image, and skips that release.
#
# The fork publishes a build only when it stands (its hooks fit upstream's release and its
# checks pass), so a release of upstream that the fork cannot follow never gets here.
#
# State: the Deployment's cliproxy/{release,image,previous,upgraded-at,verify,skip}
# annotations, and a drain marker in an emptyDir, so a keeper restart in the middle of a
# drain resumes it.
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
# Without a key, the proxy answers 401 through the gate. Any other answer (503 from the
# gate, or 200 meaning nobody is asked for a key) is unhealthy.
HEALTH_URL=${KEEPER_HEALTH_URL:?}
STATE=${KEEPER_STATE:-/run/keeper}
MARKER=$STATE/drain
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
drain_on() {
  rt 'experimental-mode on; set var proc.draining int(1)' >/dev/null &&
    rt 'disable frontend api' >/dev/null
}
drain_off() {
  rt 'enable frontend api' >/dev/null
  rt 'experimental-mode on; set var proc.draining int(0)' >/dev/null
}
conns() { rt 'show info' | awk -F': *' '$1 == "CurrConns" { print $2 }'; }

deploy_json() { kubectl -n "$NAMESPACE" get deployment "$DEPLOY" -o json; }
image_of() { jq -r --arg c "$CONTAINER" '.spec.template.spec.containers[] | select(.name == $c) | .image' <<<"$1"; }
ann_of() { jq -r --arg k "cliproxy/$2" '.metadata.annotations[$k] // ""' <<<"$1"; }
# The tag of an image the keeper knows: what is between the repository and the digest.
tag_of() {
  [[ $1 =~ $PREVIOUS_RE ]] || return 1
  local tag=${1%@*}
  printf '%s' "${tag##*:}"
}
annotate() { kubectl -n "$NAMESPACE" annotate --overwrite deployment "$DEPLOY" "$@" >/dev/null; }
set_image() {
  kubectl -n "$NAMESPACE" patch deployment "$DEPLOY" --type strategic -p "$(
    jq -nc --arg c "$CONTAINER" --arg i "$1" --argjson a "$2" \
      '{metadata: {annotations: $a}, spec: {template: {spec: {containers: [{name: $c, image: $i}]}}}}'
  )" >/dev/null
}

# The rollout deletes this pod. Keep refusing connections until it does.
await_replacement() {
  log "patched; the Recreate rollout replaces this pod next"
  for _ in $(seq 300); do
    drain_on >/dev/null 2>&1
    nap 2
  done
  log "this pod was not replaced within 10 minutes; accepting connections again"
  drain_off
  rm -f "$MARKER"
}

# switch_to MODE TARGET FROM: drain with no time limit, then patch the image.
switch_to() {
  local mode=$1 target=$2 from=$3 n=0 c ann
  printf '%s %s %s\n' "$mode" "$target" "$from" >"$MARKER"
  log "$mode: $from -> $target; refusing new connections, waiting for open ones to finish"
  while :; do
    drain_on || log "$mode: runtime API not answering; retrying"
    c=$(conns)
    [ "$c" = 0 ] && break
    if ((n++ % 30 == 0)); then log "$mode: ${c:-?} client connection(s) still open"; fi
    nap 2
  done
  ann=$(jq -nc --arg m "$mode" --arg r "$(tag_of "$target")" --arg i "$target" --arg p "$from" \
    --arg t "$(date +%s)" --arg s "$(tag_of "$from")" '
    {"cliproxy/release": $r, "cliproxy/image": $i, "cliproxy/previous": $p, "cliproxy/upgraded-at": $t}
    + if $m == "rollback" then {"cliproxy/verify": "rolled-back", "cliproxy/skip": $s}
      else {"cliproxy/verify": "pending"} end')
  log "$mode: no client connection remains; patching the image"
  if ! set_image "$target" "$ann"; then
    log "$mode: patch failed; accepting connections again"
    drain_off
    rm -f "$MARKER"
    return 1
  fi
  await_replacement
}

resume() {
  local mode target from d
  [ -s "$MARKER" ] || return 0
  read -r mode target from <"$MARKER"
  until d=$(deploy_json); do
    log "resume: cannot read the deployment; retrying"
    nap 30
  done
  if [ "$(image_of "$d")" = "$target" ]; then
    log "resume: restarted after the $mode patch"
    await_replacement
  else
    log "resume: restarted during a $mode drain; resuming it"
    switch_to "$mode" "$target" "$from"
  fi
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
  local d img target at prev
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
    switch_to rollback "$prev" "$img"
  else
    log "verify: $img failed and there is no previous image to roll back to"
    annotate cliproxy/verify=failed
  fi
}

check_release() {
  local d img cur skip release latest target
  NEXT=$INTERVAL
  if ! d=$(deploy_json); then
    log "cannot read the deployment"
    NEXT=$RETRY_WAIT
    return
  fi
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
  switch_to upgrade "$target" "$img"
}

mkdir -p "$STATE"
log "started for $NAMESPACE/$DEPLOY; release check every ${INTERVAL}s"
resume
verify_pending
while :; do
  check_release
  nap "$NEXT"
done

