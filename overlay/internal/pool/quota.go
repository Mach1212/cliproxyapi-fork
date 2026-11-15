package pool

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Weekly is what a subscription says of its weekly limit.
type Weekly struct {
	// Used is the share of the week's limit that is used, from 0 to 1.
	Used float64
	// UsedKnown is false until a subscription reports usage.
	UsedKnown bool
	// ResetAt is when the week's limit starts over, zero when not known.
	ResetAt time.Time
}

// A weekly reset is at most seven days away. The hour allows for clocks that differ.
const weekHorizon = 7*24*time.Hour + time.Hour

func signal(signals map[string]string, name string) (string, bool) {
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func unixSeconds(raw string) (time.Time, bool) {
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}

// weeklyFromSignals reads the weekly limit out of the rate-limit headers the proxy kept from
// an account's last answer. A reset that has passed means the figures are of the week
// before: the usage is then zero and the next reset unknown.
func weeklyFromSignals(provider string, signals map[string]string, now time.Time) Weekly {
	var (
		used     float64
		hasUsed  bool
		reset    time.Time
		hasReset bool
	)
	switch strings.ToLower(provider) {
	case "claude":
		if raw, ok := signal(signals, "anthropic-ratelimit-unified-7d-utilization"); ok {
			if value, err := strconv.ParseFloat(raw, 64); err == nil {
				used, hasUsed = value, true
			}
		}
		if raw, ok := signal(signals, "anthropic-ratelimit-unified-7d-reset"); ok {
			reset, hasReset = unixSeconds(raw)
		}
	case "codex":
		// Of Codex's two windows, the weekly one is the longer.
		window, longest := "", 0.0
		for _, name := range []string{"primary", "secondary"} {
			raw, ok := signal(signals, "x-codex-"+name+"-window-minutes")
			if !ok {
				continue
			}
			if minutes, err := strconv.ParseFloat(raw, 64); err == nil && minutes > longest {
				window, longest = name, minutes
			}
		}
		if window != "" {
			if raw, ok := signal(signals, "x-codex-"+window+"-used-percent"); ok {
				if value, err := strconv.ParseFloat(raw, 64); err == nil {
					used, hasUsed = value/100, true
				}
			}
			if raw, ok := signal(signals, "x-codex-"+window+"-reset-at"); ok {
				reset, hasReset = unixSeconds(raw)
			}
		}
	}
	return weekly(used, hasUsed, reset, hasReset, now)
}

func weekly(used float64, hasUsed bool, reset time.Time, hasReset bool, now time.Time) Weekly {
	if hasReset && !reset.After(now) {
		// The week these figures are of is over.
		return Weekly{Used: 0, UsedKnown: hasUsed}
	}
	out := Weekly{}
	if hasUsed {
		out.Used, out.UsedKnown = min(max(used, 0), 1), true
	}
	if hasReset && reset.Sub(now) <= weekHorizon {
		out.ResetAt = reset
	}
	return out
}

// ModelLimit is a limit the provider keeps for one model inside the week, such as Claude's
// for Fable, as the account's usage page has it.
type ModelLimit struct {
	// Model is the provider's own name for the model.
	Model string `json:"model"`
	// Used is the share of that limit that is used, from 0 to 1.
	Used    float64    `json:"used"`
	ResetAt *time.Time `json:"reset_at"`
}

// usagePage is what a provider's own usage page says of an account right now.
type usagePage struct {
	// used, hasUsed and reset are the weekly limit's figures; reset is zero when the page
	// names none.
	used    float64
	hasUsed bool
	reset   time.Time
	models  []ModelLimit
}

// namesReset tells whether the page names a weekly reset that is still to come.
func (page usagePage) namesReset(now time.Time) bool {
	return page.reset.After(now) && page.reset.Sub(now) <= weekHorizon
}

// probes holds what the usage pages said. The pool reads a page for what an account's own
// answers do not tell it: the proxy forgets an account's headers when it restarts, an account
// that serves nothing has none, Grok's answers carry no usage at all, and the limit of a
// single model, such as Fable's, is only on the page.
type probes struct {
	mu      sync.Mutex
	entries map[string]*probe
}

type probe struct {
	page usagePage
	// read is false while no read went well.
	read     bool
	triedAt  time.Time
	wait     time.Duration
	failed   bool
	inFlight bool
}

const (
	probeWait    = 10 * time.Minute
	probeWaitMax = 6 * time.Hour
)

func newProbes() *probes { return &probes{entries: map[string]*probe{}} }

// get returns the weekly limit as the account's page had it.
func (ps *probes) get(id string, now time.Time) (Weekly, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry := ps.entries[id]
	if entry == nil || !entry.read || !entry.page.namesReset(now) {
		return Weekly{}, false
	}
	return weekly(entry.page.used, entry.page.hasUsed, entry.page.reset, true, now), true
}

// modelLimits returns the limits of single models as the account's page had them, without
// those that have started over since.
func (ps *probes) modelLimits(id string, now time.Time) []ModelLimit {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	var limits []ModelLimit
	if entry := ps.entries[id]; entry != nil {
		for _, limit := range entry.page.models {
			if limit.ResetAt == nil || limit.ResetAt.After(now) {
				limits = append(limits, limit)
			}
		}
	}
	return limits
}

// weeklyOf is a subscription's weekly limit as far as the pool knows it: from the account's
// own answers first, which are newer than a page read up to ten minutes ago, else from its
// usage page.
func (p *Pool) weeklyOf(auth *coreauth.Auth, now time.Time) Weekly {
	if auth == nil {
		return Weekly{}
	}
	week := weeklyFromSignals(auth.Provider, auth.Quota.Signals, now)
	if !week.ResetAt.IsZero() {
		return week
	}
	if probed, ok := p.probes.get(auth.ID, now); ok {
		return probed
	}
	return week
}

// readUsagePages reads the usage page of every enabled Claude, Codex and Grok account that
// is due: soon after the pool starts, and then every ten minutes. A page is read through the
// proxy's management API, which puts the account's token in, so the pool never holds one.
// That is no model request and uses nothing. While a page fails or names no weekly reset,
// the wait before the next try doubles.
func (p *Pool) readUsagePages(ctx context.Context) {
	if p.cfg.ManagementKeyFile == "" {
		return
	}
	now := p.now()
	for _, auth := range p.manager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.Disabled {
			continue
		}
		source, ok := usageSources[strings.ToLower(auth.Provider)]
		if !ok || !p.probes.begin(auth.ID, now) {
			continue
		}
		page, err := p.askUsage(ctx, auth, source)
		p.probes.end(auth.ID, page, err == nil, now)
		switch {
		case err != nil:
			log.Warnf("pool: %s: its usage page could not be read: %v", auth.ID, err)
		case !page.namesReset(now):
			log.Warnf("pool: %s: its usage page names no weekly reset", auth.ID)
		}
	}
}

func (ps *probes) begin(id string, now time.Time) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry := ps.entries[id]
	if entry == nil {
		entry = &probe{}
		ps.entries[id] = entry
	}
	if entry.inFlight || now.Sub(entry.triedAt) < entry.wait {
		return false
	}
	entry.inFlight = true
	entry.triedAt = now
	return true
}

func (ps *probes) end(id string, page usagePage, read bool, now time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry := ps.entries[id]
	if entry == nil {
		return
	}
	entry.inFlight = false
	if read {
		entry.page, entry.read = page, true
		if page.namesReset(now) {
			entry.wait, entry.failed = probeWait, false
			return
		}
	}
	switch {
	case !entry.failed:
		entry.wait = probeWait
	case entry.wait*2 > probeWaitMax:
		entry.wait = probeWaitMax
	default:
		entry.wait *= 2
	}
	entry.failed = true
}

// usageSource is where a provider keeps an account's usage page and how to read it. The
// headers are what the proxy's own control panel sends, so the provider sees the same from
// both.
type usageSource struct {
	url    string
	header map[string]string
	read   func(body []byte) (usagePage, error)
}

var usageSources = map[string]usageSource{
	"claude": {
		url: "https://api.anthropic.com/api/oauth/usage",
		header: map[string]string{
			"User-Agent":     "claude-cli/2.1.280 (external, cli)",
			"Authorization":  "Bearer $TOKEN$",
			"Accept":         "application/json",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		},
		read: readClaudeUsage,
	},
	"codex": {
		url: "https://chatgpt.com/backend-api/wham/usage",
		header: map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"OpenAI-Beta":   "codex-1",
			"Originator":    "Codex Desktop",
		},
		read: readCodexUsage,
	},
	"xai": {
		url: "https://cli-chat-proxy.grok.com/v1/billing?format=credits",
		header: map[string]string{
			"Authorization":         "Bearer $TOKEN$",
			"x-xai-token-auth":      "xai-grok-cli",
			"x-grok-client-version": "1.0.77",
			"User-Agent":            "grok-cli/1.0.77",
		},
		read: readGrokUsage,
	},
}

func (p *Pool) askUsage(ctx context.Context, auth *coreauth.Auth, source usageSource) (usagePage, error) {
	header := maps.Clone(source.header)
	if account, _ := auth.Metadata["account_id"].(string); account != "" && strings.EqualFold(auth.Provider, "codex") {
		header["Chatgpt-Account-Id"] = account
	}
	request := map[string]any{
		"auth_index": auth.EnsureIndex(),
		"method":     "GET",
		"url":        source.url,
		"header":     header,
	}
	var answer struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := p.management(ctx, "POST", "/requests/api-call", request, &answer); err != nil {
		return usagePage{}, err
	}
	if answer.StatusCode != 200 {
		return usagePage{}, errStatus("it", answer.StatusCode)
	}
	return source.read([]byte(answer.Body))
}

// A page that does not look like a usage page is an error, not a page that names nothing:
// the log then says so, and what the pool read before stays.
var errNotUsagePage = errText("it is not in a shape the pool knows")

func readClaudeUsage(body []byte) (usagePage, error) {
	var raw struct {
		// Null for an account that has not used its week yet, which is not the same as a
		// page without it.
		SevenDay json.RawMessage `json:"seven_day"`
		Limits   []struct {
			Kind string `json:"kind"`
			// Percent is 0 to 100.
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || (raw.SevenDay == nil && raw.Limits == nil) {
		return usagePage{}, errNotUsagePage
	}
	var page usagePage
	var week *struct {
		// Utilization is 0 to 100.
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	if len(raw.SevenDay) > 0 {
		if err := json.Unmarshal(raw.SevenDay, &week); err != nil {
			return usagePage{}, errNotUsagePage
		}
	}
	if week != nil {
		if week.Utilization != nil {
			page.used, page.hasUsed = *week.Utilization/100, true
		}
		if at, err := time.Parse(time.RFC3339Nano, week.ResetsAt); err == nil {
			page.reset = at
		}
	}
	// The limits of single models, such as Fable's, are only in this list. One counts whether
	// or not it is the limit the account is closest to.
	for _, limit := range raw.Limits {
		if limit.Kind != "weekly_scoped" || limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		name := strings.TrimSpace(limit.Scope.Model.DisplayName)
		if name == "" {
			continue
		}
		model := ModelLimit{Model: name, Used: min(max(*limit.Percent/100, 0), 1)}
		if at, err := time.Parse(time.RFC3339Nano, limit.ResetsAt); err == nil {
			at = at.UTC()
			model.ResetAt = &at
		}
		page.models = append(page.models, model)
	}
	slices.SortStableFunc(page.models, func(a, b ModelLimit) int { return strings.Compare(a.Model, b.Model) })
	return page, nil
}

func readCodexUsage(body []byte) (usagePage, error) {
	type window struct {
		// UsedPercent is 0 to 100.
		UsedPercent *float64 `json:"used_percent"`
		Seconds     float64  `json:"limit_window_seconds"`
		// ResetAt is in Unix seconds.
		ResetAt float64 `json:"reset_at"`
	}
	var raw struct {
		// Null for an account without a limit, which is not the same as a page without it.
		RateLimit json.RawMessage `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.RateLimit == nil {
		return usagePage{}, errNotUsagePage
	}
	var limit *struct {
		Primary   *window `json:"primary_window"`
		Secondary *window `json:"secondary_window"`
	}
	if err := json.Unmarshal(raw.RateLimit, &limit); err != nil {
		return usagePage{}, errNotUsagePage
	}
	var page usagePage
	if limit == nil {
		return page, nil
	}
	// The weekly window is the one of seven days, else the longer of the two, as with the
	// headers.
	var week *window
	for _, w := range []*window{limit.Primary, limit.Secondary} {
		if w == nil {
			continue
		}
		if w.Seconds == 604800 {
			week = w
			break
		}
		if week == nil || w.Seconds > week.Seconds {
			week = w
		}
	}
	if week != nil {
		if week.UsedPercent != nil {
			page.used, page.hasUsed = *week.UsedPercent/100, true
		}
		if week.ResetAt > 0 {
			page.reset = time.Unix(int64(week.ResetAt), 0)
		}
	}
	return page, nil
}

func readGrokUsage(body []byte) (usagePage, error) {
	var raw struct {
		Config *struct {
			CurrentPeriod *struct {
				Type string `json:"type"`
				End  string `json:"end"`
			} `json:"currentPeriod"`
			// CreditUsagePercent is 0 to 100. Grok leaves it out when nothing is used.
			CreditUsagePercent float64 `json:"creditUsagePercent"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return usagePage{}, errNotUsagePage
	}
	var page usagePage
	// An account whose period is not a week has no weekly limit to report.
	if raw.Config == nil || raw.Config.CurrentPeriod == nil || raw.Config.CurrentPeriod.Type != "USAGE_PERIOD_TYPE_WEEKLY" {
		return page, nil
	}
	page.used, page.hasUsed = raw.Config.CreditUsagePercent/100, true
	if at, err := time.Parse(time.RFC3339Nano, raw.Config.CurrentPeriod.End); err == nil {
		page.reset = at
	}
	return page, nil
}

// refusesUntil tells whether the account's last answer for one of these models was a refusal
// that lasts: Anthropic says so in the answer's headers, with the time the limit starts over.
// That is how a full limit of one model shows, such as Fable's, while the account still
// serves its other models. The proxy does not set such an account aside by itself.
func refusesUntil(auth *coreauth.Auth, models []string, now time.Time) (time.Time, bool) {
	if auth == nil || !strings.EqualFold(auth.Provider, "claude") {
		return time.Time{}, false
	}
	for stateModel, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		matches := false
		for _, model := range models {
			if modelName(stateModel) == modelName(model) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		status, _ := signal(state.Quota.Signals, "anthropic-ratelimit-unified-status")
		if !strings.EqualFold(status, "rejected") {
			continue
		}
		raw, ok := signal(state.Quota.Signals, "anthropic-ratelimit-unified-reset")
		if !ok {
			continue
		}
		if until, ok := unixSeconds(raw); ok && until.After(now) {
			return until, true
		}
	}
	return time.Time{}, false
}
