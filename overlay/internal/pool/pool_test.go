package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

const siteKey = "site-key-site-key-site-key-site-key-0123"

// account is an account of the proxy as a test sets it up.
type account struct {
	id, provider string
	signals      map[string]string
	// refuses names models the account refused in its last answer, and for how long from now.
	refuses map[string]time.Duration
}

func claude(id string, used float64, resetIn time.Duration) account {
	return account{id: id, provider: "claude", signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(used, 'f', -1, 64),
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(testNow.Add(resetIn).Unix(), 10),
	}}
}

// newTestPool makes a pool over a real account manager, with its state in a fresh directory.
func newTestPool(t *testing.T, accounts ...account) *Pool {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, a := range accounts {
		auth := &coreauth.Auth{ID: a.id, Provider: a.provider, Status: coreauth.StatusActive}
		auth.Quota.Signals = a.signals
		for model, wait := range a.refuses {
			if auth.ModelStates == nil {
				auth.ModelStates = map[string]*coreauth.ModelState{}
			}
			state := &coreauth.ModelState{Status: coreauth.StatusActive}
			state.Quota.Signals = map[string]string{
				"Anthropic-Ratelimit-Unified-Status": "rejected",
				"Anthropic-Ratelimit-Unified-Reset":  strconv.FormatInt(testNow.Add(wait).Unix(), 10),
			}
			auth.ModelStates[model] = state
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("registering %s: %v", a.id, err)
		}
	}
	dir := t.TempDir()
	keyFile := dir + "/site-key"
	if err := writeFileAtomic(keyFile, []byte(siteKey+"\n")); err != nil {
		t.Fatal(err)
	}
	p, err := newPool(Config{StateDir: dir, SiteKeyFile: keyFile}, manager)
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return testNow }
	return p
}

func as(person string) context.Context {
	return coreusage.WithAPIKey(context.Background(), principalPrefix+person)
}

func pick(t *testing.T, p *Pool, ctx context.Context, model string, offered ...string) (string, error) {
	t.Helper()
	req := pluginapi.SchedulerPickRequest{Model: model}
	for _, id := range offered {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id})
	}
	resp, handled, err := (&scheduler{pool: p}).PickAuth(ctx, req)
	if !handled {
		t.Fatalf("the scheduler left the choice to someone else")
	}
	return resp.AuthID, err
}

func refusalCode(t *testing.T, err error) (string, int) {
	t.Helper()
	var refused *coreauth.Error
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	return refused.Code, refused.HTTPStatus
}

func TestTheAccountWithTheClosestResetIsUsedWhoeverOwnsIt(t *testing.T) {
	p := newTestPool(t,
		claude("far.json", 0.1, 6*24*time.Hour),
		claude("near.json", 0.9, 5*time.Hour),
		account{id: "silent.json", provider: "claude"},
	)
	// The person owns the account that resets last. Owning it does not move it forward.
	if err := p.state.setRule("far.json", Rule{Owner: "ada"}); err != nil {
		t.Fatal(err)
	}
	if err := p.state.setRule("near.json", Rule{Owner: "grace"}); err != nil {
		t.Fatal(err)
	}

	got, err := pick(t, p, as("ada"), "claude-sonnet-5", "far.json", "silent.json", "near.json")
	if err != nil || got != "near.json" {
		t.Fatalf("got %q, %v; want near.json", got, err)
	}
	// With the closest one tried already, the next closest. One that names no reset is last.
	got, _ = pick(t, p, as("ada"), "claude-sonnet-5", "far.json", "silent.json")
	if got != "far.json" {
		t.Fatalf("got %q, want far.json", got)
	}
	got, _ = pick(t, p, as("ada"), "claude-sonnet-5", "silent.json")
	if got != "silent.json" {
		t.Fatalf("got %q, want silent.json", got)
	}
}

func TestAReserveKeepsTheRestOfTheWeekForTheOwner(t *testing.T) {
	p := newTestPool(t, claude("near.json", 0.80, 5*time.Hour), claude("far.json", 0.20, 6*24*time.Hour))
	if err := p.state.setRule("near.json", Rule{Owner: "grace", ReservePercent: 20}); err != nil {
		t.Fatal(err)
	}

	// 80% used with 20% reserved: the others are passed on to the next account.
	got, err := pick(t, p, as("ada"), "claude-sonnet-5", "near.json", "far.json")
	if err != nil || got != "far.json" {
		t.Fatalf("got %q, %v; want far.json", got, err)
	}
	// The owner still uses it, and still by the order of resets.
	got, _ = pick(t, p, as("grace"), "claude-sonnet-5", "near.json", "far.json")
	if got != "near.json" {
		t.Fatalf("the owner got %q, want near.json", got)
	}
	// Just under the reserve it serves everyone.
	p2 := newTestPool(t, claude("near.json", 0.79, 5*time.Hour))
	if err := p2.state.setRule("near.json", Rule{Owner: "grace", ReservePercent: 20}); err != nil {
		t.Fatal(err)
	}
	if got, err := pick(t, p2, as("ada"), "claude-sonnet-5", "near.json"); err != nil || got != "near.json" {
		t.Fatalf("got %q, %v; want near.json", got, err)
	}

	// With nothing else to use, the person is told why and until when.
	_, err = pick(t, p, as("ada"), "claude-sonnet-5", "near.json")
	code, status := refusalCode(t, err)
	if code != "pool_reserve_reached" || status != http.StatusTooManyRequests {
		t.Fatalf("got %s %d", code, status)
	}
	if !strings.Contains(err.Error(), "2026-11-13 17:00 UTC") {
		t.Fatalf("the refusal does not say when the week starts over: %v", err)
	}
}

func TestABlockedModelIsBlockedForTheOthersIncludingLaterVersions(t *testing.T) {
	p := newTestPool(t, claude("a.json", 0.1, time.Hour), claude("b.json", 0.1, 2*time.Hour))
	if err := p.state.setRule("a.json", Rule{Owner: "grace", BlockedModels: []string{"claude-fable-*"}}); err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{"claude-fable-5-2", "claude-fable-9", "CLAUDE-FABLE-5-2", "team/claude-fable-5-2", "claude-fable-5-2(high)"} {
		got, err := pick(t, p, as("ada"), model, "a.json", "b.json")
		if err != nil || got != "b.json" {
			t.Errorf("%s: got %q, %v; want b.json", model, got, err)
		}
	}
	if got, _ := pick(t, p, as("ada"), "claude-sonnet-5", "a.json", "b.json"); got != "a.json" {
		t.Errorf("another model: got %q, want a.json", got)
	}
	if got, _ := pick(t, p, as("grace"), "claude-fable-5-2", "a.json", "b.json"); got != "a.json" {
		t.Errorf("the owner: got %q, want a.json", got)
	}
	// The model the client asked for counts too, when the proxy routes it under another name.
	req := pluginapi.SchedulerPickRequest{Model: "claude-latest", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a.json"}}}
	req.Options.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: "claude-fable-5-2"}
	if _, _, err := (&scheduler{pool: p}).PickAuth(as("ada"), req); err == nil {
		t.Errorf("a blocked model was served under the name it was routed by")
	}

	_, err := pick(t, p, as("ada"), "claude-fable-5-2", "a.json")
	if code, status := refusalCode(t, err); code != "pool_model_blocked" || status != http.StatusForbidden {
		t.Fatalf("got %s %d", code, status)
	}
}

func TestASubscriptionServesOnlyThePeopleItIsSharedWith(t *testing.T) {
	p := newTestPool(t, claude("private.json", 0, time.Hour), claude("some.json", 0, 2*time.Hour), claude("open.json", 0, 3*time.Hour))
	if err := p.state.setRule("private.json", Rule{Owner: "grace", Share: SharePrivate}); err != nil {
		t.Fatal(err)
	}
	if err := p.state.setRule("some.json", Rule{Owner: "grace", Share: SharePeople, People: []string{"ada"}}); err != nil {
		t.Fatal(err)
	}
	all := []string{"private.json", "some.json", "open.json"}

	for person, want := range map[string]string{"grace": "private.json", "ada": "some.json", "linus": "open.json"} {
		if got, err := pick(t, p, as(person), "claude-sonnet-5", all...); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %s", person, got, err, want)
		}
	}
	_, err := pick(t, p, as("linus"), "claude-sonnet-5", "private.json", "some.json")
	if code, _ := refusalCode(t, err); code != "pool_not_shared" {
		t.Fatalf("got %s", code)
	}
}

func TestKeysOfTheProxysOwnListAreTheOperators(t *testing.T) {
	p := newTestPool(t, claude("private.json", 0, time.Hour), claude("open.json", 0, 2*time.Hour))
	if err := p.state.setRule("private.json", Rule{Owner: "maciej", Share: SharePrivate}); err != nil {
		t.Fatal(err)
	}
	legacy := coreusage.WithAPIKey(context.Background(), "a-key-from-the-config-file")

	// Until an operator is named, such a key is nobody's.
	if got, _ := pick(t, p, legacy, "claude-sonnet-5", "private.json", "open.json"); got != "open.json" {
		t.Fatalf("got %q, want open.json", got)
	}
	if err := p.state.setOperator("maciej"); err != nil {
		t.Fatal(err)
	}
	if got, _ := pick(t, p, legacy, "claude-sonnet-5", "private.json", "open.json"); got != "private.json" {
		t.Fatalf("got %q, want private.json", got)
	}
	// What the proxy asks for itself, such as checking an account, is under no rule.
	if got, _ := pick(t, p, context.Background(), "claude-sonnet-5", "private.json"); got != "private.json" {
		t.Fatalf("an internal request got %q", got)
	}
}

func TestGroksFastModelsAreRefusedWhereFastModeIsOff(t *testing.T) {
	p := newTestPool(t, account{id: "grok.json", provider: "xai"})
	if err := p.state.setRule("grok.json", Rule{Owner: "maciej", BlockFastMode: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := pick(t, p, as("ada"), "grok-4.7", "grok.json"); err != nil || got != "grok.json" {
		t.Fatalf("the normal model: got %q, %v", got, err)
	}
	_, err := pick(t, p, as("ada"), "grok-4.7-build-fast", "grok.json")
	if code, _ := refusalCode(t, err); code != "pool_fast_mode_blocked" {
		t.Fatalf("got %s", code)
	}
	if got, _ := pick(t, p, as("maciej"), "grok-4.7-build-fast", "grok.json"); got != "grok.json" {
		t.Fatalf("the owner got %q", got)
	}
}

func TestWeeklyFiguresComeFromTheAccountsOwnAnswers(t *testing.T) {
	reset := testNow.Add(40 * time.Hour)
	stamp := strconv.FormatInt(reset.Unix(), 10)

	week := weeklyFromSignals("claude", map[string]string{
		"anthropic-ratelimit-unified-7d-utilization": "0.68",
		"anthropic-ratelimit-unified-7d-reset":       stamp,
	}, testNow)
	if !week.UsedKnown || week.Used != 0.68 || !week.ResetAt.Equal(reset) {
		t.Errorf("claude: %+v", week)
	}

	// Codex names two windows; the weekly one is the longer, whichever comes first.
	week = weeklyFromSignals("codex", map[string]string{
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Used-Percent":     "99",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Used-Percent":   "42",
		"X-Codex-Secondary-Reset-At":       stamp,
	}, testNow)
	if !week.UsedKnown || week.Used != 0.42 || !week.ResetAt.Equal(reset) {
		t.Errorf("codex: %+v", week)
	}

	// Figures of a week that is over: nothing is used yet, and the next reset is not known.
	week = weeklyFromSignals("claude", map[string]string{
		"anthropic-ratelimit-unified-7d-utilization": "0.97",
		"anthropic-ratelimit-unified-7d-reset":       strconv.FormatInt(testNow.Add(-time.Minute).Unix(), 10),
	}, testNow)
	if !week.UsedKnown || week.Used != 0 || !week.ResetAt.IsZero() {
		t.Errorf("a week that is over: %+v", week)
	}

	if week = weeklyFromSignals("xai", nil, testNow); week.UsedKnown || !week.ResetAt.IsZero() {
		t.Errorf("grok reports nothing: %+v", week)
	}
}

func TestFastModeIsTakenOffARequest(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"claude", `{"model":"claude-opus-5-5","speed":"fast","messages":[{"role":"user","content":"speed: fast"}]}`,
			`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"speed: fast"}]}`},
		{"codex", `{"model":"gpt-6-sol","service_tier":"priority","input":"hi"}`, `{"model":"gpt-6-sol","input":"hi"}`},
		{"codex, by its other names", `{"service_tier":"ultrafast","input":"hi"}`, `{"input":"hi"}`},
	}
	for _, c := range cases {
		got, changed := withoutFastMode([]byte(c.body))
		if !changed || string(got) != c.want {
			t.Errorf("%s: got %s", c.name, got)
		}
	}
	for _, body := range []string{
		`{"model":"claude-opus-5-5","messages":[]}`,
		`{"service_tier":"flex","input":"hi"}`,
		`{"speed":"standard"}`,
		`{"metadata":{"speed":"fast","service_tier":"priority"}}`,
		``,
	} {
		if got, changed := withoutFastMode([]byte(body)); changed || string(got) != body {
			t.Errorf("%s was changed to %s", body, got)
		}
	}
}

func TestTheStepAfterAnAccountIsChosenFollowsThatAccountsRule(t *testing.T) {
	p := newTestPool(t, claude("slow.json", 0, time.Hour), claude("any.json", 0, 2*time.Hour))
	if err := p.state.setRule("slow.json", Rule{Owner: "grace", BlockFastMode: true}); err != nil {
		t.Fatal(err)
	}
	current.Store(p)
	t.Cleanup(func() { current.Store(nil) })

	request := func(account string) cliproxyexecutor.RequestAfterAuthInterceptRequest {
		return cliproxyexecutor.RequestAfterAuthInterceptRequest{
			Body:     []byte(`{"model":"claude-opus-5-5","speed":"fast"}`),
			Headers:  http.Header{"Anthropic-Beta": {"oauth-2025-04-20, fast-mode-2026-02-01"}},
			Metadata: map[string]any{cliproxyexecutor.SelectedAuthMetadataKey: account},
		}
	}
	step := WrapAfterAuth(nil)

	resp := step(as("ada"), request("slow.json"))
	if string(resp.Body) != `{"model":"claude-opus-5-5"}` {
		t.Errorf("on the account without fast mode: body %s", resp.Body)
	}
	if got := resp.Headers.Get("Anthropic-Beta"); got != "oauth-2025-04-20" {
		t.Errorf("on the account without fast mode: betas %q", got)
	}
	for name, resp := range map[string]cliproxyexecutor.RequestAfterAuthInterceptResponse{
		"the owner":       step(as("grace"), request("slow.json")),
		"another account": step(as("ada"), request("any.json")),
	} {
		if len(resp.Body) != 0 || resp.Headers != nil {
			t.Errorf("%s: the request was changed: %s %v", name, resp.Body, resp.Headers)
		}
	}

	// What upstream's own step changed is what the pool's step works on.
	upstream := func(context.Context, cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
		return cliproxyexecutor.RequestAfterAuthInterceptResponse{Body: []byte(`{"model":"other","speed":"fast"}`)}
	}
	if resp := WrapAfterAuth(upstream)(as("ada"), request("slow.json")); string(resp.Body) != `{"model":"other"}` {
		t.Errorf("after upstream's step: %s", resp.Body)
	}
}

func TestATokenSignsItsPersonInAndIsNotKept(t *testing.T) {
	p := newTestPool(t)
	current.Store(p)
	t.Cleanup(func() { current.Store(nil) })

	token, err := p.state.mintToken("ada", "dashboard", testNow)
	if err != nil || !strings.HasPrefix(token, TokenPrefix) {
		t.Fatalf("minting: %q, %v", token, err)
	}
	saved, _ := json.Marshal(p.state.data)
	if bytes.Contains(saved, []byte(token)) {
		t.Fatalf("the token itself is in the state")
	}

	signIn := func(set func(*http.Request)) (string, error) {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		set(r)
		result, authErr := accessProvider{}.Authenticate(context.Background(), r)
		if authErr != nil {
			return "", authErr
		}
		return result.Principal, nil
	}
	for name, set := range map[string]func(*http.Request){
		"authorization": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
		"x-api-key":     func(r *http.Request) { r.Header.Set("X-Api-Key", token) },
	} {
		if principal, err := signIn(set); err != nil || principal != "pool:ada" {
			t.Errorf("%s: %q, %v", name, principal, err)
		}
	}
	if _, err := signIn(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+TokenPrefix+"made-up") }); err == nil {
		t.Errorf("a made-up token signed in")
	}
	// A key that is not the pool's is for the proxy's own key list to judge.
	foreign := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	foreign.Header.Set("Authorization", "Bearer some-config-key")
	if _, authErr := (accessProvider{}).Authenticate(context.Background(), foreign); !sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNotHandled) {
		t.Errorf("a foreign key: %v", authErr)
	}

	// A new token of the same name replaces the old one; another name does not.
	second, _ := p.state.mintToken("ada", "dashboard", testNow)
	other, _ := p.state.mintToken("ada", "t3code", testNow)
	if _, ok := p.state.lookupToken(token, testNow); ok {
		t.Errorf("the replaced token still works")
	}
	for _, live := range []string{second, other} {
		if found, ok := p.state.lookupToken(live, testNow); !ok || found.Person != "ada" {
			t.Errorf("a live token does not work")
		}
	}

	// The state on disk is what a restart starts from.
	reloaded, err := loadState(p.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.lookupToken(second, testNow); !ok || reloaded.tokenCount() != 2 {
		t.Errorf("after a restart: %d tokens", reloaded.tokenCount())
	}
}

func TestUsageIsCountedPerPersonModelSubscriptionAndWeek(t *testing.T) {
	p := newTestPool(t)
	record := func(principal, model, sub string, at time.Time, in, out int64, failed bool) {
		p.usage.HandleUsage(context.Background(), coreusage.Record{
			APIKey: principal, Model: model, AuthID: sub, RequestedAt: at, Failed: failed,
			Detail: coreusage.Detail{InputTokens: in, OutputTokens: out, TotalTokens: in + out},
		})
	}
	nextWeek := testNow.Add(7 * 24 * time.Hour)
	record("pool:ada", "claude-sonnet-5", "a.json", testNow, 100, 10, false)
	record("pool:ada", "claude-sonnet-5", "a.json", testNow, 50, 5, true)
	record("pool:ada", "claude-sonnet-5", "b.json", testNow, 7, 1, false)
	record("pool:ada", "claude-sonnet-5", "a.json", nextWeek, 1, 1, false)
	record("pool:grace", "gpt-6-sol", "c.json", testNow, 3, 2, false)
	record("", "claude-sonnet-5", "a.json", testNow, 9, 9, false)

	find := func(rows []UsageRow, person, sub, week string) Counts {
		t.Helper()
		for _, row := range rows {
			if row.Person == person && row.Subscription == sub && row.Week == week {
				return row.Counts
			}
		}
		t.Fatalf("no row for %s %s %s in %+v", person, sub, week, rows)
		return Counts{}
	}
	check := func(rows []UsageRow) {
		t.Helper()
		if len(rows) != 5 {
			t.Fatalf("%d rows: %+v", len(rows), rows)
		}
		if got := find(rows, "ada", "a.json", "2026-W46"); got != (Counts{Requests: 2, Failed: 1, Input: 150, Output: 15, Total: 165}) {
			t.Errorf("ada on a.json this week: %+v", got)
		}
		if got := find(rows, "ada", "a.json", "2026-W47"); got.Requests != 1 {
			t.Errorf("ada on a.json next week: %+v", got)
		}
		if got := find(rows, PersonInternal, "a.json", "2026-W46"); got.Total != 18 {
			t.Errorf("the proxy's own requests: %+v", got)
		}
	}
	check(p.usage.Rows())

	if err := p.usage.saveIfDirty(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadUsage(p.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	check(reloaded.Rows())
}

func TestTheSiteAPI(t *testing.T) {
	p := newTestPool(t, claude("a.json", 0.5, 30*time.Hour))
	server := httptest.NewServer(p.apiHandler())
	defer server.Close()

	call := func(key, method, path, body string) (int, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		return response.StatusCode, decoded
	}

	for _, key := range []string{"", "wrong", siteKey[:len(siteKey)-1]} {
		if status, _ := call(key, "GET", "/pool/v1/subscriptions", ""); status != http.StatusUnauthorized {
			t.Errorf("key %q: status %d", key, status)
		}
	}
	if status, _ := call("", "GET", "/pool/v1/health", ""); status != http.StatusOK {
		t.Errorf("health: %d", status)
	}

	status, body := call(siteKey, "PUT", "/pool/v1/tokens/user_ada/dashboard", "")
	token, _ := body["token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, TokenPrefix) {
		t.Fatalf("minting: %d %v", status, body)
	}
	if found, ok := p.state.lookupToken(token, testNow); !ok || found.Person != "user_ada" {
		t.Fatalf("the minted token does not sign user_ada in")
	}
	if status, _ := call(siteKey, "PUT", "/pool/v1/tokens/a%20b/dashboard", ""); status != http.StatusBadRequest {
		t.Errorf("a person id with a space: %d", status)
	}
	_, body = call(siteKey, "GET", "/pool/v1/tokens", "")
	if listed, _ := json.Marshal(body); bytes.Contains(listed, []byte(token)) || !bytes.Contains(listed, []byte("user_ada")) {
		t.Errorf("the token list: %s", listed)
	}

	rule := `{"owner":"user_ada","share":"people","people":["user_grace"],"reserve_percent":20,"blocked_models":["claude-fable-*"],"block_fast_mode":true}`
	if status, body := call(siteKey, "PUT", "/pool/v1/subscriptions/a.json/rule", rule); status != http.StatusOK {
		t.Fatalf("setting a rule: %d %v", status, body)
	}
	if got := p.state.rule("a.json"); got.Owner != "user_ada" || got.ReservePercent != 20 || !got.BlockFastMode || got.People[0] != "user_grace" {
		t.Errorf("the rule that was kept: %+v", got)
	}
	for name, bad := range map[string]string{
		"a share that does not exist": `{"share":"friends"}`,
		"a reserve over 100":          `{"reserve_percent":120}`,
		"a broken pattern":            `{"blocked_models":["claude-["]}`,
		"a field that does not exist": `{"owner":"x","admin":true}`,
	} {
		if status, _ := call(siteKey, "PUT", "/pool/v1/subscriptions/a.json/rule", bad); status != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, status)
		}
	}
	if status, _ := call(siteKey, "PUT", "/pool/v1/subscriptions/nope.json/rule", `{}`); status != http.StatusNotFound {
		t.Errorf("a rule for an account that does not exist: %d", status)
	}

	_, body = call(siteKey, "GET", "/pool/v1/subscriptions", "")
	subs, _ := body["subscriptions"].([]any)
	if len(subs) != 1 {
		t.Fatalf("subscriptions: %v", body)
	}
	sub := subs[0].(map[string]any)
	weekly := sub["weekly"].(map[string]any)
	if sub["id"] != "a.json" || sub["provider"] != "claude" || weekly["used"] != 0.5 || weekly["reset_at"] != "2026-11-14T18:00:00Z" {
		t.Errorf("the subscription: %v", sub)
	}
	if sub["rule"].(map[string]any)["owner"] != "user_ada" {
		t.Errorf("the subscription's rule: %v", sub["rule"])
	}

	if status, _ := call(siteKey, "PUT", "/pool/v1/operator", `{"person":"user_ada"}`); status != http.StatusOK || p.state.operator() != "user_ada" {
		t.Errorf("setting the operator: %d, %q", status, p.state.operator())
	}
	if status, body := call(siteKey, "DELETE", "/pool/v1/tokens/user_ada/dashboard", ""); status != http.StatusOK || body["removed"] != float64(1) {
		t.Errorf("revoking: %d %v", status, body)
	}
	if _, ok := p.state.lookupToken(token, testNow); ok {
		t.Errorf("the revoked token still works")
	}
}

func TestSigningASubscriptionInGoesThroughTheManagementAPI(t *testing.T) {
	const managementKey = "the-management-key"
	var calls []string
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+managementKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/oauth/auth-url":
			_, _ = w.Write([]byte(`{"status":"ok","url":"https://claude.ai/oauth/authorize?state=S1","state":"S1"}`))
		case r.URL.Path == "/oauth/callback":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/oauth/status" && len(calls) < 4:
			_, _ = w.Write([]byte(`{"status":"wait"}`))
		case r.URL.Path == "/oauth/status":
			_, _ = w.Write([]byte(`{"status":"ok","auth_id":"claude-ada@example.com.json","saved":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer management.Close()

	p := newTestPool(t)
	p.cfg.ManagementURL = management.URL
	p.cfg.ManagementKeyFile = p.cfg.StateDir + "/management-key"
	if err := writeFileAtomic(p.cfg.ManagementKeyFile, []byte(managementKey+"\n")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := p.startSignin(ctx, "gemini", "", Rule{}); !errors.Is(err, errUnknownProvider) {
		t.Fatalf("a provider the pool does not add: %v", err)
	}
	start, err := p.startSignin(ctx, "claude", "", Rule{Owner: "ada", ReservePercent: 30})
	if err != nil || start.State != "S1" || start.Flow != "paste" || !strings.HasPrefix(start.URL, "https://claude.ai/") {
		t.Fatalf("starting: %+v, %v", start, err)
	}

	if err := p.signinCallback(ctx, "S1", "http://localhost:54545/callback?code=abc&state=OTHER"); err == nil {
		t.Errorf("an address of another sign-in was accepted")
	}
	if err := p.signinCallback(ctx, "S1", "http://localhost:54545/callback?state=S1"); err == nil {
		t.Errorf("an address without a code was accepted")
	}
	if err := p.signinCallback(ctx, "nope", "http://localhost:54545/callback?code=abc&state=nope"); !errors.Is(err, errUnknownSignin) {
		t.Errorf("a sign-in that was never started: %v", err)
	}
	if err := p.signinCallback(ctx, "S1", "http://localhost:54545/callback?code=abc&state=S1"); err != nil {
		t.Fatalf("handing the address over: %v", err)
	}

	if status, err := p.signinStatus(ctx, "S1"); err != nil || status.Status != "wait" {
		t.Fatalf("while it is not done: %+v, %v", status, err)
	}
	status, err := p.signinStatus(ctx, "S1")
	if err != nil || status.Status != "ok" || status.Subscription != "claude-ada@example.com.json" {
		t.Fatalf("when it is done: %+v, %v", status, err)
	}
	// The new account has the rule the sign-in was started with.
	if got := p.state.rule("claude-ada@example.com.json"); got.Owner != "ada" || got.ReservePercent != 30 {
		t.Errorf("the new account's rule: %+v", got)
	}
	if _, err := p.signinStatus(ctx, "S1"); !errors.Is(err, errUnknownSignin) {
		t.Errorf("a finished sign-in is still known: %v", err)
	}
	want := []string{
		"POST /oauth/auth-url?provider=claude",
		"POST /oauth/callback",
		"GET /oauth/status?state=S1",
		"GET /oauth/status?state=S1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls to the management API:\n%s", strings.Join(calls, "\n"))
	}
}

func TestAGoogleSubscriptionIsSignedInAsAntigravity(t *testing.T) {
	var calls []string
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/oauth/auth-url":
			_, _ = w.Write([]byte(`{"status":"ok","url":"https://accounts.google.com/o/oauth2/v2/auth?state=G1","state":"G1"}`))
		case "/oauth/callback":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer management.Close()
	p := newTestPool(t)
	p.cfg.ManagementURL, p.cfg.ManagementKeyFile = management.URL, p.cfg.SiteKeyFile
	ctx := context.Background()

	start, err := p.startSignin(ctx, "antigravity", "", Rule{Owner: "ada"})
	if err != nil || start.State != "G1" || start.Flow != "paste" {
		t.Fatalf("starting: %+v, %v", start, err)
	}
	// Google's address carries the scopes, and a code with a slash in it.
	if err := p.signinCallback(ctx, "G1", "http://localhost:51121/oauth-callback?state=G1&code=4%2F0AbCd&scope=email%20https://www.googleapis.com/auth/cloud-platform"); err != nil {
		t.Fatalf("handing the address over: %v", err)
	}
	if got, want := strings.Join(calls, "\n"), "POST /oauth/auth-url?provider=antigravity\nPOST /oauth/callback"; got != want {
		t.Errorf("calls to the management API:\n%s", got)
	}
}

func TestASignInTheProxyDoesNotNameGetsTheAccountThatIsNew(t *testing.T) {
	// Upstream names the account it saved only for Codex and for an account signed in again.
	var states, statusCalls int
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/auth-url":
			states++
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "url": "https://accounts.google.com/o/oauth2/v2/auth", "state": "G" + strconv.Itoa(states)})
		case "/oauth/status":
			statusCalls++
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer management.Close()
	p := newTestPool(t, account{id: "antigravity-old.json", provider: "antigravity"})
	p.cfg.ManagementURL, p.cfg.ManagementKeyFile = management.URL, p.cfg.SiteKeyFile
	ctx := context.Background()
	register := func(id, provider string) {
		t.Helper()
		if _, err := p.manager.Register(ctx, &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}); err != nil {
			t.Fatal(err)
		}
	}

	start, err := p.startSignin(ctx, "antigravity", "", Rule{Owner: "ada"})
	if err != nil {
		t.Fatal(err)
	}
	// Saved, but not among the accounts yet.
	if status, err := p.signinStatus(ctx, start.State); err != nil || status.Status != "wait" {
		t.Fatalf("before the account is there: %+v, %v", status, err)
	}
	register("claude-new.json", "claude")
	register("antigravity-new.json", "antigravity")
	status, err := p.signinStatus(ctx, start.State)
	if err != nil || status.Status != "ok" || status.Subscription != "antigravity-new.json" {
		t.Fatalf("once it is there: %+v, %v", status, err)
	}
	if got := p.state.rule("antigravity-new.json"); got.Owner != "ada" {
		t.Errorf("the new account's rule: %+v", got)
	}
	if got := p.state.rule("antigravity-old.json"); got.Owner != "" {
		t.Errorf("the account that was there before got a rule: %+v", got)
	}
	if statusCalls != 1 {
		t.Errorf("the proxy was asked %d times; once it has saved the account it forgets the sign-in within a minute", statusCalls)
	}

	// An account the pool has already, added again: nothing is new. After a minute the sign-in
	// says so, and no account gets its rule.
	again, err := p.startSignin(ctx, "antigravity", "", Rule{Owner: "grace"})
	if err != nil {
		t.Fatal(err)
	}
	if status, err := p.signinStatus(ctx, again.State); err != nil || status.Status != "wait" {
		t.Fatalf("within the minute: %+v, %v", status, err)
	}
	p.now = func() time.Time { return testNow.Add(signinSettle) }
	if status, err := p.signinStatus(ctx, again.State); err != nil || status.Status != "error" || !strings.Contains(status.Error, "in the pool already") {
		t.Fatalf("after the minute: %+v, %v", status, err)
	}
	for _, id := range []string{"antigravity-old.json", "antigravity-new.json"} {
		if got := p.state.rule(id); got.Owner == "grace" {
			t.Errorf("%s got the rule of a sign-in that did not make it", id)
		}
	}

	// A subscription signed in again is the one it names, and keeps its rule.
	renewed, err := p.startSignin(ctx, "antigravity", "antigravity-new.json", Rule{})
	if err != nil {
		t.Fatal(err)
	}
	if status, err := p.signinStatus(ctx, renewed.State); err != nil || status.Status != "ok" || status.Subscription != "antigravity-new.json" {
		t.Fatalf("signed in again: %+v, %v", status, err)
	}
	if got := p.state.rule("antigravity-new.json"); got.Owner != "ada" {
		t.Errorf("the rule after signing in again: %+v", got)
	}
}

func TestAnAccountThatRefusesTheModelIsAskedLast(t *testing.T) {
	near := claude("near.json", 0.86, 5*time.Hour)
	near.refuses = map[string]time.Duration{"claude-fable-5-2": 5 * time.Hour}
	stale := claude("stale.json", 0.5, 2*24*time.Hour)
	stale.refuses = map[string]time.Duration{"claude-fable-5-2": -time.Hour}
	p := newTestPool(t, near, stale, claude("far.json", 0.33, 6*24*time.Hour))
	all := []string{"far.json", "stale.json", "near.json"}

	// Fable's limit is full on the account that resets first, so the next closest is asked.
	// A refusal whose time has passed counts for nothing.
	if got, _ := pick(t, p, as("ada"), "claude-fable-5-2", all...); got != "stale.json" {
		t.Errorf("fable: got %q, want stale.json", got)
	}
	// The same account still comes first for the models it serves.
	if got, _ := pick(t, p, as("ada"), "claude-haiku-4-5", all...); got != "near.json" {
		t.Errorf("haiku: got %q, want near.json", got)
	}
	// With nothing else left it is asked after all: what it said may be out of date.
	if got, err := pick(t, p, as("ada"), "claude-fable-5-2", "near.json"); err != nil || got != "near.json" {
		t.Errorf("fable, as the last one left: got %q, %v", got, err)
	}
}
