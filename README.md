# CLIProxyAPI, with a pool

This is a fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
It runs upstream's releases with one thing added: the proxy's accounts become a **pool** of
subscriptions that several people share, each under the rule of its owner.

What the pool adds:

- **Tokens that belong to a person.** The proxy knows who a request is from, and keeps the
  person's id where it would otherwise keep the key.
- **A rule per subscription**, set by its owner: who may use it, the share of the week kept
  for the owner, the share of Claude's Fable limit kept for the owner, blocked models, fast
  mode. The rule binds everyone but the owner.
- **Which subscription serves a request:** of those the person may use, the one whose weekly
  reset is closest, whoever owns it.
- **Usage figures** per person, model and subscription, by week.
- **The site API**, on a port of its own, through which a website runs all of this:
  [docs/site-api.md](docs/site-api.md).

The pool is off unless `POOL_STATE_DIR` is set. Without it a build of this fork behaves as
upstream's.

## What it publishes

For each release of upstream, a [release here](../../releases) named `<upstream tag>-pool.<n>`
and the image `ghcr.io/mach1212/cliproxyapi-fork:<that name>`, for linux/amd64 and
linux/arm64. The release's notes carry the line `image: <name>@sha256:…`.
[`builds.md`](builds.md) lists the builds.

The image is laid out like upstream's (`eceasy/cli-proxy-api`): a deployment of upstream's
image takes this one unchanged.

## How it follows upstream

[A workflow](.github/workflows/follow-upstream.yml) runs every hour. It asks upstream
for its latest release and, if this fork has no build of it with the current pool, runs
[`build.sh`](build.sh) and publishes the result.

This branch, `pool`, holds only what the fork adds. Upstream's source is fetched at the
release's tag for each build:

- [`overlay/`](overlay) is copied over it: the pool itself (`internal/pool`) and one small
  file beside upstream's scheduler. Upstream has neither.
- [`hooks.sh`](hooks.sh) makes upstream's code call the pool at three places. Each change
  hangs on one line of upstream's code and is made only when that line is there exactly once.

A build is published only when it stands:

1. the hooks fit;
2. the pool's own tests pass;
3. upstream's tests of the packages the hooks touch pass, or fail on upstream's untouched
   source too (upstream has tests that fail by themselves now and then);
4. the built program passes an end-to-end run against a stand-in for Anthropic, which checks
   from the outside that each hook is in place.

When a release does not stand, that run fails once, an issue is opened here and assigned to
the fork's owner, and no build of it appears until the fork's files change or upstream
releases again. Earlier builds stay as
they are, so a proxy that follows this fork's releases stays on the last one that stood.

## Building one yourself

Needs git, a C compiler and the Go that upstream's `go.mod` names.

```sh
./build.sh v8.0.72 out                       # fetch, hook, check; leaves out/src and out/cli-proxy-api
docker buildx build -f Dockerfile out/src    # the image
```

`build.sh` exits 3 when a hook does not fit the release.

## Settings

The pool is set up with environment variables, so upstream's configuration file keeps its
shape:

| variable | what it is |
|---|---|
| `POOL_STATE_DIR` | Where the pool keeps `state.json` (tokens as hashes, rules) and `usage.json`. Setting it turns the pool on. Keep it on a disk that lasts. |
| `POOL_LISTEN` | The site API's address. Default `:8320`; empty for no site API. |
| `POOL_SITE_KEY_FILE` | A file with the site API's key, at least 32 characters. |
| `POOL_MANAGEMENT_URL` | The proxy's own management API, default `http://127.0.0.1:8317/v8/management`. The pool calls it over loopback to sign subscriptions in, to remove one, and to read an account's usage page for its weekly usage, its weekly reset and the limits of single models. |
| `POOL_MANAGEMENT_KEY_FILE` | A file with the management key in clear. Without it those three things do not work; the rest does. |

With the pool on:

- a request needs a key: one of the pool's tokens, or one of `access.api-keys`. The proxy
  never serves without one, also when `access.api-keys` is empty;
- the pool chooses the account for every request. `routing.strategy`, account priorities and
  session affinity no longer decide, and a scheduler plugin is not asked;
- the proxy does not start when the pool cannot: a state file it cannot read, or no site key.

## Deploying it

[`chart/cliproxyapi`](chart/cliproxyapi) is a Helm chart for the proxy with the pool, in two
pods:

- **the proxy's pod**: the proxy on loopback, and beside it the relay, the one listener the
  gate reaches;
- **the gate's pod**: the gate, the only listener clients reach, and the keeper, which keeps
  the proxy on this fork's latest build.

The gate is a pod of its own so that its port stays open while the proxy's pod restarts. A
request that arrives meanwhile waits for the proxy instead of being refused: the gate and the
relay each try their next hop about once a second, `gate.retries` times.

```sh
helm upgrade --install cliproxyapi chart/cliproxyapi --namespace cliproxyapi -f my-values.yaml
```

What has to exist first, in the namespace (the chart makes neither: both hold keys):

- **The configuration**, a Secret (`config.existingSecret`, default `cliproxyapi-config`) with
  upstream's `config.yaml` and, as `management-key`, the management key in clear. In
  `config.yaml` the proxy listens on `127.0.0.1` and on `proxy.port` (8318), with
  `management.secret-key` set and `management.allow-remote: false`.
- **The site key**, a Secret (`pool.siteKey.existingSecret`, default `cliproxyapi-pool`) with
  `site-key`: at least 32 characters, which the website holds too.
- Nothing for the image: it is public. (`imagePullSecrets` is there for a private copy.)

Create Secrets with `kubectl create` or `kubectl apply --server-side`, never with a plain
`kubectl apply`, which copies their values into an annotation.

Values worth setting:

- `networkPolicy.clientCIDRs`: who reaches the gate. `networkPolicy.siteFrom`: the website's
  pods, which reach the site API. Nothing else gets in.
- `gate.hostPort`: clients reach the gate at its pod's node, port 8317. Pin the pods to that
  node with `affinity` or `nodeSelector`, which place both.
- `auth.existingClaim`: the disk of signed-in accounts, when there is one already.

The route from outside the cluster is yours to make; the chart has none. Send it to the gate
(port 8317), and never cut an answer short: a model answer streams for minutes.

Things to know:

- **Upgrading the chart leaves the proxy's image alone** once it runs a build of this fork:
  the keeper moves it, and an upgrade must not undo that. `image.force: true` sets it.
- **A new build goes in without a refused or a cut request.** The keeper waits for a moment
  when no client connection is open. Nobody waits for that moment, so under steady load a
  build waits. Then the keeper holds the gate, sets the image, and opens the gate once the
  new proxy answers: a request that arrives meanwhile waits those seconds.
- **A build that does not answer, or fails its check, is taken out and skipped.** The
  Deployment's `cliproxy/skip` annotation names it, and the keeper leaves that release alone
  until a newer one is out. Remove the annotation to let the keeper try it again; set it to
  keep the keeper from a release.
- **An upgrade that changes the proxy's pod restarts it at once**, and cuts the answers in
  flight; requests that arrive meanwhile wait at the gate. To spare the answers, do it at a
  moment without a client connection, the way the keeper does (`files/keeper.sh`, `quiet`).
- **An upgrade that changes the gate's pod** (the gate's configuration, the keeper's script)
  restarts it at once too, and with a port of the node that port is closed for a few
  seconds. Do it when nobody is working.
- **The relay's port is for the gate's pod alone**, and the chart's NetworkPolicy is what
  closes it to other pods. On a cluster that does not enforce NetworkPolicy every pod
  reaches it; the relay then still lets only `/v1/` and `/v1beta/` paths through, as the
  gate does.
- **The two disks stay** when the release is removed (`helm.sh/resource-policy: keep`).
- **The keeper's admission policy** is a cluster-wide object named after the namespace and
  the release. It lists the pod fields of Kubernetes v1.35.
