#!/usr/bin/env bash
# Makes one upstream release of CLIProxyAPI into this fork's build of it: upstream's source at
# that tag with the pool put in (hooks.sh), checked, and left ready for the image build.
#
#   build.sh <tag> <out-dir>        for example: build.sh v8.0.72 out
#
# Leaves <out-dir>/src (the source the image is built from) and <out-dir>/cli-proxy-api (the
# program the checks ran against). Exit 3 means a hook does not fit this release; any other
# failure is a check's.
#
# The checks: the pool's own tests, the tests of the upstream packages the hooks touch, and
# an end-to-end run of the built program against a stand-in for Anthropic.
#
# Needs git, go (the version upstream's go.mod names) and a C compiler.
set -euo pipefail

tag=${1:?usage: build.sh <tag> <out-dir>}
mkdir -p "${2:?usage: build.sh <tag> <out-dir>}"
out=$(cd "$2" && pwd)
here=$(cd "$(dirname "$0")" && pwd)
upstream=https://github.com/router-for-me/CLIProxyAPI.git

say() { printf '\n== %s\n' "$*"; }

say "upstream's source of $tag"
rm -rf "$out/src"
git clone --quiet --depth 1 --branch "$tag" "$upstream" "$out/src"
git -C "$out/src" log -1 --format='%h %cs %s'

say "the pool's hooks"
"$here/hooks.sh" "$out/src"

cd "$out/src"
say "the pool's own tests"
go vet ./internal/pool/
go test -count=1 ./internal/pool/

say "upstream's tests of what the hooks touch"
# Upstream has tests that fail now and then by themselves, and now and then a release whose
# own tests fail. So a failing test counts against the pool only when it fails twice here and
# passes on upstream's untouched source.
packages="./sdk/api/handlers/ ./sdk/cliproxy/ ./sdk/cliproxy/auth/ ./sdk/access/... ./internal/access/..."
# failing DIR [go test flags]: prints the names of the tests that fail, one per line.
failing() {
  local dir=$1 log status=0
  shift
  log=$(mktemp)
  # shellcheck disable=SC2086 # the packages are several words
  (cd "$dir" && go test -count=1 "$@" $packages) > "$log" 2>&1 || status=$?
  if [ "$status" != 0 ] && ! grep -q '^--- FAIL: ' "$log"; then
    tail -n 40 "$log" >&2
    echo "the tests did not run" >&2
    exit 1
  fi
  sed -n 's/^--- FAIL: \([^ ]*\).*/\1/p' "$log" | sort -u
  rm -f "$log"
}
pattern() { printf '^(%s)$' "$(paste -sd '|')"; }
failed=$(failing "$out/src")
if [ -n "$failed" ]; then
  echo "failed once: $(echo $failed)"
  failed=$(failing "$out/src" -run "$(pattern <<< "$failed")")
fi
if [ -n "$failed" ]; then
  echo "failed twice: $(echo $failed); asking upstream's untouched source"
  rm -rf "$out/untouched"
  git clone --quiet --depth 1 --branch "$tag" "$upstream" "$out/untouched"
  theirs=$(failing "$out/untouched" -run "$(pattern <<< "$failed")")
  rm -rf "$out/untouched"
  ours=$(comm -23 <(echo "$failed") <(echo "$theirs"))
  if [ -n "$ours" ]; then
    echo "these tests of upstream pass without the pool and fail with it: $(echo $ours)" >&2
    exit 1
  fi
  echo "upstream's own source fails them too: not the pool's"
fi

say "the program"
CGO_ENABLED=1 go build -buildvcs=false -o "$out/cli-proxy-api" ./cmd/server

say "end to end"
POOL_E2E_BINARY=$out/cli-proxy-api go test -count=1 -tags poole2e -run TestEndToEnd ./internal/pool/

say "$tag stands with the pool"
