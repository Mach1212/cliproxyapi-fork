#!/usr/bin/env bash
# Puts the pool into a checkout of upstream CLIProxyAPI: copies overlay/ over it (files
# upstream does not have) and makes upstream's own code call the pool at three places.
#
#   hooks.sh <upstream checkout>
#
# Each of the three changes hangs on one line of upstream's code, its anchor. A change is
# made only when its anchor is in the file exactly once; otherwise this script says which one
# is not, and exits 3: a hook does not fit this release, and someone has to look.
#
# The hooks:
#   1. sdk/cliproxy/auth/conductor_selection.go, SetPluginScheduler: the scheduler the proxy
#      is given goes through the pool first, so the pool chooses the account for a request.
#   2. sdk/cliproxy/service_lifecycle.go, Run: the pool starts with the service, and the
#      service does not start when the pool cannot.
#   3. sdk/api/handlers/handlers_interceptors.go, requestAfterAuthInterceptor: the pool's
#      step runs once an account is chosen, before the request is translated for it.
set -euo pipefail

tree=${1:?usage: hooks.sh <upstream checkout>}
here=$(cd "$(dirname "$0")" && pwd)
module=github.com/router-for-me/CLIProxyAPI/v8

misfit() {
  echo "hooks: $*" >&2
  exit 3
}

# replace FILE ANCHOR REPLACEMENT: the one line equal to ANCHOR becomes REPLACEMENT, which
# may be several lines.
replace() {
  local file=$tree/$1 anchor=$2 replacement=$3 count
  [ -f "$file" ] || misfit "$1 is gone"
  count=$(grep -c -x -F -- "$anchor" "$file" || true)
  [ "$count" = 1 ] || misfit "$1: expected this line once, found it $count times: $anchor"
  ANCHOR=$anchor REPLACEMENT=$replacement awk '
    $0 == ENVIRON["ANCHOR"] { print ENVIRON["REPLACEMENT"]; next }
    { print }
  ' "$file" > "$file.hooked"
  mv "$file.hooked" "$file"
}

# import FILE PACKAGE: adds an import to the file's first import block. gofmt puts it in order.
import() {
  local file=$tree/$1 count
  count=$(grep -c -x -F 'import (' "$file" || true)
  [ "$count" -ge 1 ] || misfit "$1 has no import block"
  IMPORT=$2 awk '
    !done && $0 == "import (" { print; print "\t\"" ENVIRON["IMPORT"] "\""; done = 1; next }
    { print }
  ' "$file" > "$file.hooked"
  mv "$file.hooked" "$file"
}

[ -f "$tree/go.mod" ] && grep -q -x -F "module $module" "$tree/go.mod" ||
  misfit "$tree is not a checkout of $module"
for path in internal/pool sdk/cliproxy/auth/pool_hook.go; do
  [ ! -e "$tree/$path" ] || misfit "upstream has its own $path now"
done
cp -R "$here/overlay/." "$tree/"

tab=$(printf '\t')

replace sdk/cliproxy/auth/conductor_selection.go \
  "${tab}m.pluginScheduler = scheduler" \
  "${tab}m.pluginScheduler = decorateScheduler(scheduler)"

replace sdk/cliproxy/service_lifecycle.go \
  "${tab}usage.StartDefault(ctx)" \
  "${tab}usage.StartDefault(ctx)
${tab}if errPool := pool.Start(ctx, s.coreManager, s.accessManager); errPool != nil {
${tab}${tab}return errPool
${tab}}"
import sdk/cliproxy/service_lifecycle.go "$module/internal/pool"

signature='func (h *BaseAPIHandler) requestAfterAuthInterceptor(capture *requestAfterAuthCapture, requestID, skipPluginID string) coreexecutor.RequestAfterAuthInterceptor {'
replace sdk/api/handlers/handlers_interceptors.go \
  "$signature" \
  "$signature
${tab}return pool.WrapAfterAuth(h.pluginRequestAfterAuthInterceptor(capture, requestID, skipPluginID))
}

${signature/requestAfterAuthInterceptor/pluginRequestAfterAuthInterceptor}"
import sdk/api/handlers/handlers_interceptors.go "$module/internal/pool"

gofmt -w \
  "$tree/sdk/cliproxy/auth/conductor_selection.go" \
  "$tree/sdk/cliproxy/service_lifecycle.go" \
  "$tree/sdk/api/handlers/handlers_interceptors.go"
echo "hooks: in place"
