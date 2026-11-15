package pool

import (
	"context"
	"encoding/json"
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
	// UsedKnown is false for a subscription that reports no usage, such as Grok.
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

// probes holds, for accounts whose answers have not said it yet, the weekly limit read from
// the provider's own usage page. The proxy forgets an account's headers when it restarts, and
// an account that serves nothing has none; without this such an account would stay last in
// line for ever.
type probes struct {
	mu      sync.Mutex
	entries map[string]*probe
}

type probe struct {
	used     float64
	hasUsed  bool
	reset    time.Time
	triedAt  time.Time
	wait     time.Duration
	inFlight bool
}

const (
	probeWait    = 10 * time.Minute
	probeWaitMax = 6 * time.Hour
)

func newProbes() *probes { return &probes{entries: map[string]*probe{}} }

func (ps *probes) get(id string, now time.Time) (Weekly, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry := ps.entries[id]
	if entry == nil || entry.reset.IsZero() || !entry.reset.After(now) {
		return Weekly{}, false
	}
	return weekly(entry.used, entry.hasUsed, entry.reset, true, now), true
}

// weeklyOf is a subscription's weekly limit as far as the pool knows it: from the account's
// own answers first, else from its usage page.
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

// probeUnknownResets asks the usage page of each Claude account that has no known weekly
// reset. It goes through the proxy's management API, which puts the account's token in, so
// the pool never holds one. That is no model request and uses nothing. While a page names no
// reset, the wait before the next try doubles.
func (p *Pool) probeUnknownResets(ctx context.Context) {
	if p.cfg.ManagementKeyFile == "" {
		return
	}
	now := p.now()
	for _, auth := range p.manager.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, "claude") {
			continue
		}
		if !weeklyFromSignals(auth.Provider, auth.Quota.Signals, now).ResetAt.IsZero() {
			continue
		}
		if _, ok := p.probes.get(auth.ID, now); ok {
			continue
		}
		if !p.probes.begin(auth.ID, now) {
			continue
		}
		used, hasUsed, reset, err := p.askClaudeUsage(ctx, auth.EnsureIndex())
		p.probes.end(auth.ID, used, hasUsed, reset, now)
		if err != nil {
			log.Warnf("pool: %s: its weekly reset is not known: %v", auth.ID, err)
		} else {
			log.Infof("pool: %s: weekly reset %s, read from its usage page", auth.ID, reset.UTC().Format("2006-01-02 15:04 MST"))
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

func (ps *probes) end(id string, used float64, hasUsed bool, reset time.Time, now time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry := ps.entries[id]
	if entry == nil {
		return
	}
	entry.inFlight = false
	if !reset.IsZero() && reset.After(now) && reset.Sub(now) <= weekHorizon {
		entry.used, entry.hasUsed, entry.reset, entry.wait = used, hasUsed, reset, 0
		return
	}
	entry.reset = time.Time{}
	switch {
	case entry.wait == 0:
		entry.wait = probeWait
	case entry.wait*2 > probeWaitMax:
		entry.wait = probeWaitMax
	default:
		entry.wait *= 2
	}
}

const (
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	// What the proxy's own control panel sends, so Anthropic sees the same from both.
	claudeUsageAgent = "claude-cli/2.1.280 (external, cli)"
)

func (p *Pool) askClaudeUsage(ctx context.Context, authIndex string) (used float64, hasUsed bool, reset time.Time, err error) {
	request := map[string]any{
		"auth_index": authIndex,
		"method":     "GET",
		"url":        claudeUsageURL,
		"header": map[string]string{
			"User-Agent":     claudeUsageAgent,
			"Authorization":  "Bearer $TOKEN$",
			"Accept":         "application/json",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		},
	}
	var answer struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := p.management(ctx, "POST", "/requests/api-call", request, &answer); err != nil {
		return 0, false, time.Time{}, err
	}
	if answer.StatusCode != 200 {
		return 0, false, time.Time{}, errStatus("its usage page", answer.StatusCode)
	}
	var page struct {
		SevenDay struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.Unmarshal([]byte(answer.Body), &page); err != nil {
		return 0, false, time.Time{}, errText("its usage page could not be read")
	}
	at, errParse := time.Parse(time.RFC3339Nano, page.SevenDay.ResetsAt)
	if errParse != nil {
		return 0, false, time.Time{}, errText("its usage page names no weekly reset")
	}
	if page.SevenDay.Utilization != nil {
		// The page gives percent.
		used, hasUsed = *page.SevenDay.Utilization/100, true
	}
	return used, hasUsed, at, nil
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
