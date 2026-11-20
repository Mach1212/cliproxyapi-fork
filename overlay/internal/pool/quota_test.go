package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// usagePages stands in for the management API where it asks a provider's usage page: it
// answers with the page the test has put up at that address, and keeps what it was asked.
type usagePages struct {
	pages map[string]string
	asked []map[string]any
}

func serveUsagePages(t *testing.T, p *Pool) *usagePages {
	t.Helper()
	pages := &usagePages{pages: map[string]string{}}
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call map[string]any
		if r.URL.Path != "/requests/api-call" || json.NewDecoder(r.Body).Decode(&call) != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		pages.asked = append(pages.asked, call)
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": pages.pages[call["url"].(string)]})
	}))
	t.Cleanup(management.Close)
	p.cfg.ManagementURL = management.URL
	p.cfg.ManagementKeyFile = p.cfg.StateDir + "/management-key"
	if err := writeFileAtomic(p.cfg.ManagementKeyFile, []byte("the-management-key\n")); err != nil {
		t.Fatal(err)
	}
	return pages
}

func TestUsagePagesReportWeeklyFigures(t *testing.T) {
	for _, test := range []struct {
		name, provider, body string
		used                 float64
		known                bool
		reset                string
		fails                bool
	}{
		{"claude", "claude", `{"seven_day":{"utilization":47.0,"resets_at":"2026-11-21T02:59:59.620375+00:00"},"limits":[{"kind":"weekly_all","group":"weekly","percent":47,"resets_at":"2026-11-21T02:59:59.620375+00:00","scope":null,"is_active":false},{"kind":"weekly_scoped","group":"weekly","percent":93,"resets_at":"2026-11-21T02:59:59.620515+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":true}]}`, .47, true, "2026-11-21T02:59:59.620375Z", false},
		{"claude null", "claude", `{"seven_day":null,"limits":null}`, 0, false, "", false},
		{"claude missing week", "claude", `{"limits":[{"kind":"weekly_scoped","percent":93,"scope":{"model":{"display_name":"Fable"}}}]}`, 0, false, "", false},
		{"claude missing percent", "claude", `{"seven_day":{"resets_at":"2026-11-21T02:59:59Z"}}`, 0, false, "2026-11-21T02:59:59Z", false},
		{"claude null fields", "claude", `{"seven_day":{"utilization":null,"resets_at":null}}`, 0, false, "", false},
		{"claude missing", "claude", `{}`, 0, false, "", true},
		{"codex", "codex", `{"plan_type":"prolite","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_after_seconds":478101,"reset_at":1795233222},"secondary_window":null}}`, .01, true, "2026-11-21T03:53:42Z", false},
		{"codex secondary week", "codex", `{"rate_limit":{"primary_window":{"used_percent":99,"limit_window_seconds":18000},"secondary_window":{"used_percent":42,"limit_window_seconds":604800,"reset_at":1795233222}}}`, .42, true, "2026-11-21T03:53:42Z", false},
		{"codex explicit week before longer", "codex", `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_at":1795233222},"secondary_window":{"used_percent":99,"limit_window_seconds":1209600}}}`, .12, true, "2026-11-21T03:53:42Z", false},
		{"codex explicit week after longer", "codex", `{"rate_limit":{"primary_window":{"used_percent":99,"limit_window_seconds":1209600},"secondary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_at":1795233222}}}`, .12, true, "2026-11-21T03:53:42Z", false},
		{"codex longer fallback", "codex", `{"rate_limit":{"primary_window":{"used_percent":99,"limit_window_seconds":3600},"secondary_window":{"used_percent":22,"limit_window_seconds":86400,"reset_at":1795233222}}}`, .22, true, "2026-11-21T03:53:42Z", false},
		{"codex primary fallback", "codex", `{"rate_limit":{"primary_window":{"used_percent":23,"limit_window_seconds":86400},"secondary_window":{"used_percent":99,"limit_window_seconds":3600}}}`, .23, true, "", false},
		{"codex windows null", "codex", `{"rate_limit":{"primary_window":null,"secondary_window":null}}`, 0, false, "", false},
		{"codex windows missing", "codex", `{"rate_limit":{}}`, 0, false, "", false},
		{"codex rate null", "codex", `{"rate_limit":null}`, 0, false, "", false},
		{"codex rate missing", "codex", `{}`, 0, false, "", true},
		{"codex percent null", "codex", `{"rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":604800}}}`, 0, false, "", false},
		{"grok", "xai", `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-10T03:26:18.478515+00:00","end":"2026-11-17T03:26:18.478515+00:00"},"creditUsagePercent":8.0}}`, .08, true, "2026-11-17T03:26:18.478515Z", false},
		{"grok omitted zero", "xai", `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-17T03:26:18.478515+00:00"}}}`, 0, true, "2026-11-17T03:26:18.478515Z", false},
		{"grok nonweekly", "xai", `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_MONTHLY","end":"2026-11-17T03:26:18Z"},"creditUsagePercent":8}}`, 0, false, "", false},
		{"grok period null", "xai", `{"config":{"currentPeriod":null}}`, 0, false, "", false},
		{"grok period missing", "xai", `{"config":{}}`, 0, false, "", false},
		{"grok config null", "xai", `{"config":null}`, 0, false, "", false},
		{"grok missing", "xai", `{}`, 0, false, "", false},
		{"google", "antigravity", `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-11-18T08:00:00Z","remainingFraction":0.5}]},{"displayName":"Gemini Models","description":"Models within this group: Gemini 4 Neon","buckets":[{"bucketId":"gemini-5h","displayName":"Five Hour Limit","window":"5h","resetTime":"2026-11-13T15:00:00Z","remainingFraction":0.125},{"bucketId":"gemini-weekly","displayName":"Weekly Limit","window":"weekly","resetTime":"2026-11-19T19:29:48Z","remainingFraction":0.75}]}]}`, .25, true, "2026-11-19T19:29:48Z", false},
		{"google gemini by bucket", "antigravity", `{"groups":[{"displayName":"Pro models","buckets":[{"bucketId":"gemini-weekly","window":"Weekly","resetTime":"2026-11-19T19:29:48Z","remainingFraction":"0.75"}]}]}`, .25, true, "2026-11-19T19:29:48Z", false},
		{"google used up", "antigravity", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-11-19T19:29:48Z","remainingFraction":0}]}]}`, 1, true, "2026-11-19T19:29:48Z", false},
		{"google without gemini", "antigravity", `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.5}]}]}`, 0, false, "", false},
		{"google five hours only", "antigravity", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5}]}]}`, 0, false, "", false},
		{"google fraction missing", "antigravity", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-11-19T19:29:48Z"}]}]}`, 0, false, "", false},
		{"google first week without fraction", "antigravity", `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly"},{"bucketId":"gemini-weekly-2","window":"weekly","resetTime":"2026-11-19T19:29:48Z","remainingFraction":0.75}]}]}`, .25, true, "2026-11-19T19:29:48Z", false},
		{"google no groups", "antigravity", `{}`, 0, false, "", false},
		{"malformed claude", "claude", `{`, 0, false, "", true},
		{"malformed codex", "codex", `{`, 0, false, "", true},
		{"malformed grok", "xai", `{`, 0, false, "", true},
		{"malformed google", "antigravity", `{"groups":{}}`, 0, false, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := usageSources[test.provider].read([]byte(test.body))
			if (err != nil) != test.fails {
				t.Fatalf("reading: %v", err)
			}
			reset := ""
			if !page.reset.IsZero() {
				reset = page.reset.UTC().Format(time.RFC3339Nano)
			}
			if page.used != test.used || page.hasUsed != test.known || reset != test.reset {
				t.Errorf("got %v %v %q; want %v %v %q", page.used, page.hasUsed, reset, test.used, test.known, test.reset)
			}
		})
	}
}

func TestClaudeModelLimitsKeepNamedNumericWeeklyScopes(t *testing.T) {
	page, err := readClaudeUsage([]byte(`{"seven_day":null,"limits":[
		{"kind":"weekly_scoped","percent":93,"resets_at":"2026-11-21T02:59:59.620515+00:00","scope":{"model":{"display_name":"Fable"}},"is_active":false},
		{"kind":"weekly_scoped","percent":125,"scope":{"model":{"display_name":"Zebra"}}},
		{"kind":"weekly_scoped","percent":-4,"resets_at":null,"scope":{"model":{"display_name":"Alpha"}}},
		{"kind":"weekly_all","percent":47,"scope":{"model":{"display_name":"Ignored"}}},
		{"kind":"weekly_scoped","percent":null,"scope":{"model":{"display_name":"Null"}}},
		{"kind":"weekly_scoped","scope":{"model":{"display_name":"Missing"}}},
		{"kind":"weekly_scoped","percent":3,"scope":null},
		{"kind":"weekly_scoped","percent":3},
		{"kind":"weekly_scoped","percent":3,"scope":{"model":null}},
		{"kind":"weekly_scoped","percent":3,"scope":{}},
		{"kind":"weekly_scoped","percent":3,"scope":{"model":{"display_name":""}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 21, 2, 59, 59, 620515000, time.UTC)
	want := []ModelLimit{{Model: "Alpha", Used: 0}, {Model: "Fable", Used: .93, ResetAt: &at}, {Model: "Zebra", Used: 1}}
	if !reflect.DeepEqual(page.models, want) {
		t.Errorf("model limits: %+v; want %+v", page.models, want)
	}
}

func TestGoogleModelLimitsAreTheWeeksOfItsOtherGroups(t *testing.T) {
	page, err := readAntigravityUsage([]byte(`{"groups":[
		{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.75}]},
		{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":0.125},{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-11-18T08:00:00Z","remainingFraction":0.5}]},
		{"displayName":"Image models","buckets":[{"bucketId":"image-weekly","window":"weekly","remainingFraction":1.5}]},
		{"displayName":"Five hours only","buckets":[{"bucketId":"x-5h","window":"5h","remainingFraction":0.5}]},
		{"displayName":" ","buckets":[{"bucketId":"y-weekly","window":"weekly","remainingFraction":0.5}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	want := []ModelLimit{{Model: "Claude and GPT models", Used: .5, ResetAt: &at}, {Model: "Image models", Used: 0}}
	if !reflect.DeepEqual(page.models, want) {
		t.Errorf("model limits: %+v; want %+v", page.models, want)
	}
}

func TestGoogleUsageAsksGooglesHostsInTurnUntilOneHasGemini(t *testing.T) {
	p := newTestPool(t, account{id: "google", provider: "antigravity"})
	pages := serveUsagePages(t, p)
	// Without the project the quota is asked for, Google is not asked at all.
	p.readUsagePages(context.Background())
	if len(pages.asked) != 0 {
		t.Fatalf("asked without a project: %v", pages.asked)
	}

	auth, _ := p.manager.GetByID("google")
	auth.Metadata = map[string]any{"project_id": "proj-1"}
	if _, err := p.manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	hosts := usageSources["antigravity"].urls
	full := `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-11-19T19:29:48Z","remainingFraction":0.75}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-11-18T08:00:00Z","remainingFraction":0.25}]}]}`
	// The daily host leaves Gemini's group out, the sandbox host answers nothing the pool can
	// read, and the production host has it all.
	pages.pages[hosts[0]] = `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-11-18T08:00:00Z","remainingFraction":0.5}]}]}`
	pages.pages[hosts[2]] = full
	p.now = func() time.Time { return testNow.Add(probeWait) }
	p.readUsagePages(context.Background())
	if len(pages.asked) != 3 {
		t.Fatalf("calls: %d; want one to each host", len(pages.asked))
	}
	for i, call := range pages.asked {
		header := call["header"].(map[string]any)
		if call["url"] != hosts[i] || call["method"] != "POST" || call["data"] != `{"project":"proj-1"}` ||
			header["Authorization"] != "Bearer $TOKEN$" || header["Content-Type"] != "application/json" || header["User-Agent"] != "antigravity/hub/2.23.0 darwin/arm64" {
			t.Errorf("management request %d: %v", i, call)
		}
	}
	sub := p.subscriptions()[0]
	if sub.Weekly.Used != .25 || !sub.Weekly.UsedKnown || sub.Weekly.ResetAt == nil || !sub.Weekly.ResetAt.Equal(time.Date(2026, 11, 19, 19, 29, 48, 0, time.UTC)) {
		t.Errorf("week: %+v", sub.Weekly)
	}
	// What the first host said of a group stays.
	at := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	if want := []ModelLimit{{Model: "Claude and GPT models", Used: .5, ResetAt: &at}}; !reflect.DeepEqual(sub.ModelLimits, want) {
		t.Errorf("model limits: %+v; want %+v", sub.ModelLimits, want)
	}

	// A host that has Gemini's group is the last one asked.
	pages.asked, pages.pages[hosts[0]] = nil, full
	p.now = func() time.Time { return testNow.Add(2 * probeWait) }
	p.readUsagePages(context.Background())
	if len(pages.asked) != 1 {
		t.Errorf("calls once the daily host has it all: %d", len(pages.asked))
	}
}

func TestWeeklyHeadersPrecedePagesUntilTheirResetPasses(t *testing.T) {
	for _, test := range []struct {
		name                   string
		headerReset, pageReset time.Duration
		used                   float64
		reset                  time.Duration
	}{
		{"future headers", time.Hour, 2 * time.Hour, .8, time.Hour},
		{"expired headers", -time.Hour, 2 * time.Hour, .2, 2 * time.Hour},
		{"expired page", -time.Hour, -time.Minute, 0, 0},
		{"page beyond horizon", -time.Hour, weekHorizon + time.Second, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPool(t, claude("a", .8, test.headerReset))
			p.probes.begin("a", testNow)
			p.probes.end("a", usagePage{used: .2, hasUsed: true, reset: testNow.Add(test.pageReset)}, true, testNow)
			auth, _ := p.manager.GetByID("a")
			got := p.weeklyOf(auth, testNow)
			wantReset := time.Time{}
			if test.reset != 0 {
				wantReset = testNow.Add(test.reset)
			}
			if !got.UsedKnown || got.Used != test.used || !got.ResetAt.Equal(wantReset) {
				t.Errorf("week: %+v", got)
			}
		})
	}
}

func TestSubscriptionsIncludeOnlyCurrentModelLimits(t *testing.T) {
	p := newTestPool(t, account{id: "a", provider: "claude"}, account{id: "b", provider: "codex"})
	page, err := readClaudeUsage([]byte(`{"seven_day":null,"limits":[
		{"kind":"weekly_scoped","percent":93,"resets_at":"2026-11-14T12:00:00Z","scope":{"model":{"display_name":"Fable"}}},
		{"kind":"weekly_scoped","percent":10,"resets_at":"2026-11-13T12:00:00Z","scope":{"model":{"display_name":"Expired"}}},
		{"kind":"weekly_scoped","percent":20,"scope":{"model":{"display_name":"Unknown reset"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	p.probes.begin("a", testNow)
	p.probes.end("a", page, true, testNow)
	r := httptest.NewRequest(http.MethodGet, "/pool/v1/subscriptions", nil)
	r.Header.Set("Authorization", "Bearer "+siteKey)
	w := httptest.NewRecorder()
	p.apiHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var body struct {
		Subscriptions []map[string]json.RawMessage `json:"subscriptions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Subscriptions) != 2 {
		t.Fatalf("subscriptions: %s", w.Body)
	}
	var limits []map[string]any
	if err := json.Unmarshal(body.Subscriptions[0]["model_limits"], &limits); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"model": "Fable", "used": .93, "reset_at": "2026-11-14T12:00:00Z"},
		{"model": "Unknown reset", "used": .2, "reset_at": nil},
	}
	if !reflect.DeepEqual(limits, want) {
		t.Errorf("limits: %v; want %v", limits, want)
	}
	if _, exists := body.Subscriptions[1]["model_limits"]; exists {
		t.Errorf("empty model limits were included")
	}
	p.now = func() time.Time { return testNow.Add(24 * time.Hour) }
	if limits := p.subscriptions()[0].ModelLimits; len(limits) != 1 || limits[0].Model != "Unknown reset" {
		t.Errorf("after reset: %+v", limits)
	}
}

func TestUsagePagesRefreshEnabledAccountsEvenWithKnownHeaders(t *testing.T) {
	p := newTestPool(t, claude("claude", .8, time.Hour), account{id: "codex", provider: "codex"}, account{id: "grok", provider: "xai"})
	for _, auth := range []*coreauth.Auth{
		{ID: "disabled", Provider: "claude", Disabled: true},
		{ID: "other", Provider: "gemini"},
	} {
		if _, err := p.manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	auth, _ := p.manager.GetByID("codex")
	auth.Metadata = map[string]any{"account_id": "acct-1"}
	if _, err := p.manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	pages := serveUsagePages(t, p)
	pages.pages[usageSources["claude"].urls[0]] = `{"seven_day":{"utilization":47,"resets_at":"2026-11-14T12:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":93,"scope":{"model":{"display_name":"Fable"}}}]}`
	pages.pages[usageSources["codex"].urls[0]] = `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":1794657600}}}`
	pages.pages[usageSources["xai"].urls[0]] = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-11-17T03:26:18Z"},"creditUsagePercent":8}}`
	p.readUsagePages(context.Background())
	if len(pages.asked) != 3 {
		t.Fatalf("first read: %d calls; want 3", len(pages.asked))
	}
	for _, call := range pages.asked {
		header := call["header"].(map[string]any)
		if call["method"] != "GET" || call["auth_index"] == "" || header["Authorization"] != "Bearer $TOKEN$" {
			t.Errorf("management request: %v", call)
		}
		switch call["url"] {
		case usageSources["claude"].urls[0]:
			if header["anthropic-beta"] != "oauth-2025-04-20" || header["User-Agent"] != "claude-cli/2.1.280 (external, cli)" || header["Accept"] != "application/json" || header["Content-Type"] != "application/json" {
				t.Errorf("claude headers: %v", header)
			}
		case usageSources["codex"].urls[0]:
			if header["Chatgpt-Account-Id"] != "acct-1" || header["OpenAI-Beta"] != "codex-1" || header["Originator"] != "Codex Desktop" || header["Content-Type"] != "application/json" {
				t.Errorf("codex headers: %v", header)
			}
		case usageSources["xai"].urls[0]:
			if header["x-xai-token-auth"] != "xai-grok-cli" || header["x-grok-client-version"] != "1.0.77" || header["User-Agent"] != "grok-cli/1.0.77" {
				t.Errorf("grok headers: %v", header)
			}
		default:
			t.Errorf("unexpected page: %v", call["url"])
		}
	}
	subs := p.subscriptions()
	if subs[0].Weekly.Used != .8 || len(subs[0].ModelLimits) != 1 || subs[0].ModelLimits[0].Used != .93 {
		t.Errorf("claude: %+v", subs[0])
	}
	if subs[1].Weekly.Used != .01 || !subs[1].Weekly.UsedKnown || subs[1].Weekly.ResetAt == nil {
		t.Errorf("idle codex: %+v", subs[1])
	}
	if subs[3].Weekly.Used != .08 || !subs[3].Weekly.UsedKnown || subs[3].Weekly.ResetAt == nil {
		t.Errorf("grok: %+v", subs[3])
	}
	p.now = func() time.Time { return testNow.Add(9 * time.Minute) }
	p.readUsagePages(context.Background())
	if len(pages.asked) != 3 {
		t.Errorf("before refresh: %d calls", len(pages.asked))
	}
	pages.pages[usageSources["claude"].urls[0]] = `{"seven_day":{"utilization":48,"resets_at":"2026-11-14T12:00:00Z"},"limits":[{"kind":"weekly_scoped","percent":94,"scope":{"model":{"display_name":"Fable"}}}]}`
	p.now = func() time.Time { return testNow.Add(probeWait) }
	p.readUsagePages(context.Background())
	if len(pages.asked) != 6 {
		t.Errorf("after refresh: %d calls", len(pages.asked))
	}
	if got := p.subscriptions()[0].ModelLimits[0].Used; got != .94 {
		t.Errorf("refreshed model usage: %v", got)
	}
}

func TestUsageProbeBackoffAndInFlight(t *testing.T) {
	for _, read := range []bool{false, true} {
		ps := newProbes()
		now := testNow
		for _, wait := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour} {
			if !ps.begin("a", now) {
				t.Fatal("due read was refused")
			}
			if ps.begin("a", now.Add(24*time.Hour)) {
				t.Fatal("a second read began while the first was in flight")
			}
			ps.end("a", usagePage{}, read, now)
			if ps.begin("a", now.Add(wait-time.Second)) {
				t.Fatalf("read=%v: asked before %s", read, wait)
			}
			now = now.Add(wait)
		}
		if !ps.begin("a", now) {
			t.Fatal("read at capped backoff was refused")
		}
		ps.end("a", usagePage{reset: now.Add(time.Hour)}, true, now)
		now = now.Add(probeWait)
		if !ps.begin("a", now) {
			t.Fatal("successful read did not restore ten-minute interval")
		}
		ps.end("a", usagePage{}, false, now)
		if !ps.begin("a", now.Add(probeWait)) {
			t.Fatal("first failure after success did not start at ten minutes")
		}
	}
}

func TestCodexUsageOmitsAnAbsentAccountIDAndRejectsFailedPages(t *testing.T) {
	for _, status := range []int{200, 401, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := newTestPool(t, account{id: "codex", provider: "codex"})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Header map[string]string `json:"header"`
				}
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if _, ok := call.Header["Chatgpt-Account-Id"]; ok {
					t.Error("absent account id was sent")
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_at":1795233222}}}`}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			p.cfg.ManagementURL, p.cfg.ManagementKeyFile = server.URL, p.cfg.SiteKeyFile
			auth, _ := p.manager.GetByID("codex")
			page, err := p.askUsage(context.Background(), auth, usageSources["codex"])
			if (err != nil) != (status != 200) {
				t.Fatalf("status %d: %v", status, err)
			}
			if status == 200 && (page.used != .01 || !page.hasUsed) {
				t.Errorf("page: %+v", page)
			}
		})
	}
}
