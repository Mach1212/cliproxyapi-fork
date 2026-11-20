package pool

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// scheduler chooses the account for each request. The proxy hands it the accounts that can
// serve the request right now (signed in, not cooling down, not tried yet for this request).
// It leaves out the ones the person may not use, and of the rest takes the one whose weekly
// reset is closest, whoever owns it: what would be lost first is used first.
type scheduler struct {
	pool *Pool
}

// SchedulerWantsAcrossPriorities asks the proxy for every account, not just the ones of the
// highest priority: the order is the pool's, by reset.
func (s *scheduler) SchedulerWantsAcrossPriorities() bool { return true }

type candidate struct {
	id   string
	week Weekly
	// refuses is set when the account's last answer for this model was a refusal that lasts.
	refuses bool
}

// byClosestReset orders accounts with a known weekly reset first, the closest first, then
// the others. An account that refuses the model until its limit starts over comes after all
// that do not: it is asked only when nothing else is left, in case what it said is out of
// date. Ties go by id, so the order is the same every time.
func byClosestReset(candidates []candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch {
		case a.refuses != b.refuses:
			return !a.refuses
		case a.week.ResetAt.IsZero() != b.week.ResetAt.IsZero():
			return !a.week.ResetAt.IsZero()
		case !a.week.ResetAt.Equal(b.week.ResetAt):
			return a.week.ResetAt.Before(b.week.ResetAt)
		default:
			return a.id < b.id
		}
	})
}

func requestedModels(req pluginapi.SchedulerPickRequest) []string {
	models := []string{req.Model}
	if requested, ok := req.Options.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string); ok && requested != "" && requested != req.Model {
		models = append(models, requested)
	}
	return models
}

func (s *scheduler) PickAuth(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	p := s.pool
	who := p.callerOf(ctx)
	now := p.now()
	models := requestedModels(req)

	eligible := make([]candidate, 0, len(req.Candidates))
	denials := map[denial]int{}
	// When the reserves that turned the person away start over, the soonest of each kind.
	soonest := map[denial]time.Time{}
	keepSoonest := func(why denial, at time.Time) {
		if !at.IsZero() && (soonest[why].IsZero() || at.Before(soonest[why])) {
			soonest[why] = at
		}
	}
	for _, offered := range req.Candidates {
		auth, ok := p.manager.GetByID(offered.ID)
		if !ok || auth == nil {
			continue
		}
		week := p.weeklyOf(auth, now)
		if !who.internal {
			rule := p.state.rule(auth.ID)
			var limits []ModelLimit
			if rule.FableReservePercent > 0 {
				limits = p.probes.modelLimits(auth.ID, now)
			}
			if why := rule.decide(who.person, auth.Provider, models, week, limits); why != allowed {
				denials[why]++
				switch why {
				case deniedReserve:
					keepSoonest(why, week.ResetAt)
				case deniedFableReserve:
					if fable, ok := fableLimit(limits); ok && fable.ResetAt != nil {
						keepSoonest(why, *fable.ResetAt)
					}
				}
				continue
			}
		}
		_, refuses := refusesUntil(auth, models, now)
		eligible = append(eligible, candidate{id: auth.ID, week: week, refuses: refuses})
	}
	if len(eligible) == 0 {
		return pluginapi.SchedulerPickResponse{}, true, refusal(models[0], denials, soonest)
	}
	byClosestReset(eligible)
	return pluginapi.SchedulerPickResponse{AuthID: eligible[0].id, Handled: true}, true, nil
}

// refusal is the answer to a request that no subscription may serve. It names the reason
// that leaves the person the most to do about it.
func refusal(model string, denials map[denial]int, soonest map[denial]time.Time) error {
	model = modelName(model)
	switch {
	case denials[deniedReserve] > 0:
		return reserveReached(fmt.Sprintf("The pooled subscriptions for %s have reached the share of this week that their owners keep for themselves.", model), soonest[deniedReserve])
	case denials[deniedFableReserve] > 0:
		return reserveReached(fmt.Sprintf("The pooled subscriptions for %s have reached the share of their Fable limit that their owners keep for themselves.", model), soonest[deniedFableReserve])
	case denials[deniedFastModel] > 0:
		return &coreauth.Error{Code: "pool_fast_mode_blocked", Message: fmt.Sprintf("%s is a fast model, and the pooled subscriptions do not allow fast mode. Use the model without \"-fast\".", model), HTTPStatus: http.StatusForbidden}
	case denials[deniedModel] > 0:
		return &coreauth.Error{Code: "pool_model_blocked", Message: fmt.Sprintf("No pooled subscription lets you use %s: its owners have switched it off.", model), HTTPStatus: http.StatusForbidden}
	case denials[deniedNotShared] > 0:
		return &coreauth.Error{Code: "pool_not_shared", Message: fmt.Sprintf("No subscription for %s is shared with you.", model), HTTPStatus: http.StatusForbidden}
	default:
		return &coreauth.Error{Code: "auth_not_found", Message: "no auth available"}
	}
}

// reserveReached refuses a request that only the owners' reserves stand in the way of, and
// says when the first of them starts over, if that is known.
func reserveReached(message string, soonest time.Time) error {
	if !soonest.IsZero() {
		message += " The next one starts over at " + soonest.UTC().Format("2006-01-02 15:04 MST") + "."
	}
	return &coreauth.Error{Code: "pool_reserve_reached", Message: message, HTTPStatus: http.StatusTooManyRequests}
}
