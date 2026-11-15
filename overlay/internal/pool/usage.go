package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Names the usage figures use for requests that are nobody's.
const (
	// PersonInternal is the proxy itself, for example checking an account.
	PersonInternal = "internal"
	// PersonUnknown is a key from upstream's own key list while no operator is set.
	PersonUnknown = "unknown"
)

// Counts is what a person used of one model on one subscription in one week.
type Counts struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
	// The token counts are the proxy's own, as it reads them from each provider's answer.
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	Total      int64 `json:"total_tokens"`
}

// UsageRow is one line of the usage figures.
type UsageRow struct {
	Person       string `json:"person"`
	Model        string `json:"model"`
	Subscription string `json:"subscription"`
	// Week is the ISO week the requests were made in, UTC, such as "2026-W46".
	Week string `json:"week"`
	Counts
}

type usageKey struct{ person, model, subscription, week string }

// Usage counts tokens. The proxy tells it about every request once it has ended. The counts
// are kept per week for good: the lifetime figures are their sum.
type Usage struct {
	mu     sync.Mutex
	path   string
	rows   map[usageKey]*Counts
	dirty  bool
	person func(principal string) caller
}

type usageFile struct {
	Version int        `json:"version"`
	Rows    []UsageRow `json:"rows"`
}

func loadUsage(dir string) (*Usage, error) {
	u := &Usage{path: filepath.Join(dir, "usage.json"), rows: map[usageKey]*Counts{}}
	raw, err := os.ReadFile(u.path)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", u.path, err)
	}
	var file usageFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", u.path, err)
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("%s has version %d, this build reads version 1", u.path, file.Version)
	}
	for _, row := range file.Rows {
		counts := row.Counts
		u.rows[usageKey{row.Person, row.Model, row.Subscription, row.Week}] = &counts
	}
	return u, nil
}

// WeekOf names the ISO week of a time, in UTC.
func WeekOf(at time.Time) string {
	year, week := at.UTC().ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// HandleUsage is called by the proxy for every finished request.
func (u *Usage) HandleUsage(_ context.Context, record coreusage.Record) {
	who := u.person(record.APIKey)
	person := who.person
	switch {
	case who.internal:
		person = PersonInternal
	case person == "":
		person = PersonUnknown
	}
	model := modelName(record.Model)
	if model == "" {
		model = modelName(record.Alias)
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	key := usageKey{person, model, record.AuthID, WeekOf(at)}
	detail := record.Detail

	u.mu.Lock()
	defer u.mu.Unlock()
	counts := u.rows[key]
	if counts == nil {
		counts = &Counts{}
		u.rows[key] = counts
	}
	counts.Requests++
	if record.Failed {
		counts.Failed++
	}
	counts.Input += detail.InputTokens
	counts.Output += detail.OutputTokens
	counts.CacheRead += detail.CacheReadTokens
	counts.CacheWrite += detail.CacheCreationTokens
	counts.Reasoning += detail.ReasoningTokens
	counts.Total += detail.TotalTokens
	u.dirty = true
}

// Rows returns every line, in a fixed order.
func (u *Usage) Rows() []UsageRow {
	u.mu.Lock()
	rows := make([]UsageRow, 0, len(u.rows))
	for key, counts := range u.rows {
		rows = append(rows, UsageRow{Person: key.person, Model: key.model, Subscription: key.subscription, Week: key.week, Counts: *counts})
	}
	u.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.Week != b.Week:
			return a.Week > b.Week
		case a.Person != b.Person:
			return a.Person < b.Person
		case a.Subscription != b.Subscription:
			return a.Subscription < b.Subscription
		default:
			return a.Model < b.Model
		}
	})
	return rows
}

func (u *Usage) saveIfDirty() error {
	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	u.dirty = false
	u.mu.Unlock()
	raw, err := json.Marshal(usageFile{Version: 1, Rows: u.Rows()})
	if err == nil {
		err = writeFileAtomic(u.path, raw)
	}
	if err != nil {
		u.mu.Lock()
		u.dirty = true
		u.mu.Unlock()
	}
	return err
}
