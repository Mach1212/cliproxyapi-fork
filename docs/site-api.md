# The site API

How a website runs the pool. It listens on its own port (`POOL_LISTEN`, default `:8320`),
apart from the proxy's port for model requests and apart from the management API.

- Every call but `/pool/v1/health` needs `Authorization: Bearer <site key>`. The site key is
  the content of the file `POOL_SITE_KEY_FILE`, at least 32 characters.
- Bodies and answers are JSON. A body with a field the call does not know is refused.
- A failure answers `{"error": "<what went wrong>"}`: 400 for a request that is wrong, 401
  without the site key, 404 for something that does not exist, 502 when the proxy's own
  management API failed.
- The pool does what the site key asks. Who may ask for what is the site's to decide: that
  only a subscription's owner changes its rule, for example.

It cannot read an account's login, change the proxy's configuration, or call a provider with
an account's token. Those stay with the management API, which the site never gets.

## People

A person is an id the site chooses: 1 to 128 of `a-z A-Z 0-9 _ - .`. The pool keeps no
names. A token signs its person in on the proxy's model endpoints, in any of the places a key
goes (`Authorization: Bearer`, `x-api-key`, `x-goog-api-key`, `?key=`).

A key from the proxy's own list (`access.api-keys` in its configuration) still works. Such
keys are the **operator's**: the person named with `PUT /pool/v1/operator`. Until one is
named they are nobody's, and use only what is shared with everyone.

## Tokens

| call | does |
|---|---|
| `PUT /pool/v1/tokens/{person}/{name}` | Makes a token and answers `{"token": "m12_…"}`. It replaces the person's token of that name. The token is shown this once; the pool keeps its SHA-256. |
| `GET /pool/v1/tokens` | `{"tokens": [{"person", "name", "created_at", "last_used_at"}]}` |
| `DELETE /pool/v1/tokens/{person}/{name}` | Ends that token. `{"removed": 1}` |
| `DELETE /pool/v1/tokens/{person}` | Ends all of the person's tokens. |

`name` tells a person's tokens apart, such as `dashboard` and `t3code`.

## Subscriptions

`GET /pool/v1/subscriptions` lists the proxy's accounts:

```json
{"subscriptions": [{
  "id": "claude-ada@example.com.json",
  "provider": "claude",
  "kind": "oauth",
  "email": "ada@example.com",
  "disabled": false,
  "status": "active",
  "weekly": {"used": 0.68, "used_known": true, "reset_at": "2026-11-16T23:00:00Z"},
  "model_limits": [{"model": "Fable", "used": 0.93, "reset_at": "2026-11-21T02:59:59Z"}],
  "rule": {"owner": "user_ada", "share": "everyone", "reserve_percent": 20,
           "blocked_models": ["claude-fable-*"], "block_fast_mode": true}
}]}
```

- `weekly.used` is the share of the week's limit that is used, 0 to 1, and counts only with
  `used_known`. Claude, Codex, Grok and Google accounts report it, including accounts that
  have served no model request since startup. A reserve applies to each when usage is known.
  Google keeps its quota in groups of models; a Google account's week is the weekly window
  of its Gemini models.
- `weekly.reset_at` is `null` until a weekly reset is known. Readings whose reset has
  passed are dropped. Resets more than seven days and one hour away are not used.
- The pool reads each enabled Claude, Codex, Grok and Google account's own usage page within
  about a minute of startup and refreshes it about every ten minutes. This requires
  `POOL_MANAGEMENT_KEY_FILE`. Failed reads and pages without a weekly reset back off,
  doubling the wait up to six hours. Disabled accounts are not read, nor Google accounts
  without the Google Cloud project the proxy finds when they are signed in.
- The figures of an account's own answers count while the weekly reset they name is still
  ahead: they are newer than a page read up to ten minutes ago. Otherwise the page's do.
- `model_limits` comes from the usage page and is omitted when empty. Each entry has
  `model`, the provider's display name, `used`, a share from 0 to 1, and `reset_at`,
  an RFC 3339 time or `null` when unknown. Claude reports its weekly limits for individual
  models, such as Fable, whether the provider marks the limit active or inactive. Google
  reports the weekly window of each of its groups of models besides Gemini's, named as Google
  names the group: `Claude and GPT models`, and on a Pro plan `Gemini 4 Argon`.
  Entries are sorted by model name and dropped once their reset has passed.

### The rule

`PUT /pool/v1/subscriptions/{id}/rule` sets a subscription's rule, whole:

| field | meaning |
|---|---|
| `owner` | The person it belongs to. The rule binds everyone else: the owner always may use their own subscription, any model, at any speed. |
| `share` | `everyone` (also when empty), `people`, or `private`. |
| `people` | Who may use it when `share` is `people`. |
| `reserve_percent` | 0 to 100. The others are refused once the subscription's weekly usage reaches 100 minus this. |
| `blocked_models` | Patterns of model names the others may not use, with `*` for any run of characters: `claude-fable-*`. Matched without regard to case, an account prefix (`team/model`) or a thinking suffix (`model(high)`). A model released later is blocked as soon as its name matches. |
| `block_fast_mode` | The others' requests run at normal speed: the fast setting is taken off the request (Claude's `speed`, Codex's `service_tier`). Grok's fast mode is a model of its own, named `…-fast`; those are refused. |

A subscription without a rule is nobody's and open to everyone.

`DELETE /pool/v1/subscriptions/{id}` takes the account out of the proxy, login and all.

### Which subscription serves a request

Of the accounts that can serve it right now and that the person may use, the one whose
weekly reset is closest, whoever owns it. Accounts that have not said their reset come last.
The rule decides only whether an account is among them, never the order.

One thing comes before the reset: a Claude account whose last answer for the model was a
refusal that lasts until its limit starts over (a full Fable limit, say) is asked after the
accounts that did not refuse, and still serves its other models first.

When none is left, the request is refused with a message that says why:

| status | `code` | when |
|---|---|---|
| 429 | `pool_reserve_reached` | the owners' reserves are reached; the message names the next reset |
| 403 | `pool_model_blocked` | the model is blocked on every subscription the person may use |
| 403 | `pool_fast_mode_blocked` | a Grok fast model, where fast mode is off |
| 403 | `pool_not_shared` | no subscription for the model is shared with the person |

## Adding a subscription

`POST /pool/v1/signins` starts a sign-in:

```json
{"provider": "claude", "rule": {"owner": "user_ada", "share": "everyone"}}
```

`provider` is `claude`, `codex`, `xai` or `antigravity`. `antigravity` is a Google
subscription (Google AI Pro or Ultra), signed in the way Google's Antigravity signs in; its
accounts serve Gemini, and the Claude and GPT models Google offers with it. With
`"subscription": "<id>"` instead of a rule, an existing account is signed in again and keeps
its rule. The answer:

```json
{"state": "…", "url": "https://…", "flow": "paste"}
{"state": "…", "url": "https://…", "flow": "code", "user_code": "ABCD-1234", "expires_in": 600}
```

- `paste` (Claude, Codex, Google): the person opens `url` and signs in. Their browser then
  ends at an address on their own computer that shows an error page. They copy that address, and the
  site sends it: `POST /pool/v1/signins/{state}/callback` with `{"redirect_url": "http://localhost:…?code=…&state=…"}`.
- `code` (Grok): the person opens `url` and enters `user_code`. Nothing comes back by hand.

`GET /pool/v1/signins/{state}` tells how far it is: `{"status": "wait"}`, then once
`{"status": "ok", "subscription": "<id>"}` or `{"status": "error", "error": "…"}`. With `ok`
the new account has the rule the sign-in was started with, and the sign-in is forgotten.
`DELETE /pool/v1/signins/{state}` gives it up. A sign-in not finished in 20 minutes is
forgotten.

The proxy names the account it saved only for Codex and for an account signed in again. For
the others the pool takes the account of that provider that was not there when the sign-in
started. When it cannot tell which one that is within a minute of the proxy saving it (an
account that is in the pool already, or two added at once), the sign-in ends with an error
and no account gets its rule.

## Usage

`GET /pool/v1/usage` answers every count there is, newest week first:

```json
{"week": "2026-W46", "rows": [{
  "person": "user_ada", "model": "claude-sonnet-5", "subscription": "claude-ada@example.com.json",
  "week": "2026-W46", "requests": 12, "failed": 1,
  "input_tokens": 9000, "output_tokens": 700, "cache_read_tokens": 0,
  "cache_write_tokens": 0, "reasoning_tokens": 0, "total_tokens": 9700
}]}
```

- A row is one person, one model, one subscription, one ISO week (UTC). Lifetime figures are
  the sum over the weeks; weeks are kept for good.
- `person` is `unknown` for a key of the proxy's own list while no operator is named, and
  `internal` for what the proxy asks for itself.
- `subscription` is empty in a row the proxy counted for a request that no account served.
- The counts are saved every five seconds, so a crash can lose the last few seconds.

## The rest

| call | does |
|---|---|
| `GET /pool/v1/models` | `{"models": [{"id", "owned_by"}]}`: the models the accounts serve right now. The proxy refreshes its list by itself. |
| `GET /pool/v1/operator`, `PUT /pool/v1/operator` | `{"person": "…"}`: whose the keys of the proxy's own list are. Empty for nobody. |
| `GET /pool/v1/health` | `{"status": "ok"}`, without a key. |
