package pool

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// Who may use a subscription besides its owner.
const (
	ShareEveryone = "everyone"
	SharePeople   = "people"
	SharePrivate  = "private"
)

// Rule is what a subscription's owner decided about it. The rule binds everyone but the
// owner: the owner always may use their own subscription, at any speed, for any model.
// The zero Rule is a subscription nobody owns and everybody may use.
type Rule struct {
	// Owner is the person the subscription belongs to, empty for none.
	Owner string `json:"owner"`
	// Share is ShareEveryone (also when empty), SharePeople or SharePrivate.
	Share string `json:"share"`
	// People may use the subscription when Share is SharePeople.
	People []string `json:"people,omitempty"`
	// ReservePercent of the week is kept for the owner: the others are refused once the
	// subscription's weekly usage reaches 100 minus this.
	ReservePercent int `json:"reserve_percent"`
	// FableReservePercent of a Claude subscription's Fable limit is kept for the owner: the
	// others are refused the Fable models once that limit's usage reaches 100 minus this.
	FableReservePercent int `json:"fable_reserve_percent"`
	// BlockedModels are patterns of model names the others may not use, such as
	// "claude-fable-*". A model released later is blocked as soon as its name matches.
	BlockedModels []string `json:"blocked_models,omitempty"`
	// BlockFastMode makes the others' requests run at normal speed.
	BlockFastMode bool `json:"block_fast_mode"`
}

func (r Rule) validate() error {
	switch r.Share {
	case "", ShareEveryone, SharePeople, SharePrivate:
	default:
		return fmt.Errorf("share is %q, it has to be %q, %q or %q", r.Share, ShareEveryone, SharePeople, SharePrivate)
	}
	if r.ReservePercent < 0 || r.ReservePercent > 100 {
		return fmt.Errorf("reserve_percent is %d, it has to be between 0 and 100", r.ReservePercent)
	}
	if r.FableReservePercent < 0 || r.FableReservePercent > 100 {
		return fmt.Errorf("fable_reserve_percent is %d, it has to be between 0 and 100", r.FableReservePercent)
	}
	for _, pattern := range r.BlockedModels {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("blocked_models has an empty pattern")
		}
		if _, err := path.Match(strings.ToLower(pattern), "x"); err != nil {
			return fmt.Errorf("blocked_models: %q is not a valid pattern", pattern)
		}
	}
	return nil
}

// Why a subscription does not serve a request.
type denial int

const (
	allowed denial = iota
	deniedNotShared
	deniedModel
	deniedFastModel
	deniedReserve
	deniedFableReserve
)

// fableModels are the models that count against Claude's Fable limit.
const fableModels = "claude-fable-*"

func isFableModel(model string) bool {
	ok, _ := path.Match(fableModels, modelName(model))
	return ok
}

// fableLimit is the Fable limit among an account's limits of single models. Claude names it
// "Fable"; the case of the name is not relied on.
func fableLimit(limits []ModelLimit) (ModelLimit, bool) {
	for _, limit := range limits {
		if strings.EqualFold(limit.Model, "Fable") {
			return limit, true
		}
	}
	return ModelLimit{}, false
}

// modelName is the name a rule's patterns are matched against: lower case, without an
// account prefix ("team/model") and without a thinking suffix ("model(high)").
func modelName(model string) string {
	name := strings.ToLower(strings.TrimSpace(model))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if paren := strings.Index(name, "("); paren >= 0 {
		name = name[:paren]
	}
	return name
}

func (r Rule) blocksModel(model string) bool {
	name := modelName(model)
	if name == "" {
		return false
	}
	for _, pattern := range r.BlockedModels {
		if ok, _ := path.Match(strings.ToLower(strings.TrimSpace(pattern)), name); ok {
			return true
		}
	}
	return false
}

// isFastModel tells the models that are fast mode by name. Grok has no fast setting in the
// request: its fast mode is a model of its own.
func isFastModel(provider, model string) bool {
	return strings.EqualFold(provider, "xai") && strings.HasSuffix(modelName(model), "-fast")
}

// decide tells whether the subscription serves this person's request. An empty person is
// someone the pool does not know, who is nobody's owner. limits are the account's limits of
// single models; only a Fable reserve needs them.
func (r Rule) decide(person, provider string, models []string, week Weekly, limits []ModelLimit) denial {
	if person != "" && person == r.Owner {
		return allowed
	}
	switch r.Share {
	case SharePrivate:
		return deniedNotShared
	case SharePeople:
		if person == "" || !slices.Contains(r.People, person) {
			return deniedNotShared
		}
	}
	for _, model := range models {
		if r.blocksModel(model) {
			return deniedModel
		}
		if r.BlockFastMode && isFastModel(provider, model) {
			return deniedFastModel
		}
	}
	if r.ReservePercent > 0 && week.UsedKnown && week.Used*100 >= float64(100-r.ReservePercent) {
		return deniedReserve
	}
	if r.FableReservePercent > 0 && slices.ContainsFunc(models, isFableModel) {
		if fable, ok := fableLimit(limits); ok && fable.Used*100 >= float64(100-r.FableReservePercent) {
			return deniedFableReserve
		}
	}
	return allowed
}

// stripsFastMode tells whether this person's request runs at normal speed on the subscription.
func (r Rule) stripsFastMode(person string) bool {
	return r.BlockFastMode && !(person != "" && person == r.Owner)
}
