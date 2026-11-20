package pool

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A sign-in adds someone's subscription to the pool without a shell on the proxy. The proxy's
// management API does the work; the pool starts it, passes on what the person brings back,
// and gives the new account its owner and rule.
//
//   - Claude, Codex and Google: the provider's page ends at an address on the person's own
//     computer that nothing answers. They copy that address from their browser, and the site
//     hands it to SigninCallback.
//   - Grok: the provider's page asks for a code the proxy made. Nothing comes back by hand.
//
// A Google subscription is signed in the way Antigravity, Google's coding agent, signs in:
// upstream has no other way for a Google subscription since it dropped Gemini CLI's.

// Providers a subscription can be added for.
var signinProviders = map[string]bool{"claude": true, "codex": true, "xai": true, "antigravity": true}

const (
	// A sign-in that is not finished after this long is forgotten.
	signinLifetime = 20 * time.Minute
	// How long after the proxy has saved an account it did not name the pool looks for it.
	signinSettle = time.Minute
)

type pendingSignin struct {
	provider string
	rule     Rule
	started  time.Time
	// subscription is the account signed in again, empty for a new one.
	subscription string
	// had are the provider's accounts when the sign-in started. The proxy names the account
	// it saved only for Codex and for an account signed in again; a new account of the other
	// providers is the one that was not there before.
	had map[string]bool
	// saved is when the proxy said it saved an account it did not name, zero until then.
	saved time.Time
}

type signins struct {
	mu      sync.Mutex
	pending map[string]pendingSignin
}

func newSignins() *signins { return &signins{pending: map[string]pendingSignin{}} }

func (s *signins) add(state string, signin pendingSignin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, other := range s.pending {
		if signin.started.Sub(other.started) > signinLifetime {
			delete(s.pending, key)
		}
	}
	s.pending[state] = signin
}

func (s *signins) get(state string) (pendingSignin, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	signin, ok := s.pending[state]
	return signin, ok
}

func (s *signins) remove(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, state)
}

func (s *signins) markSaved(state string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if signin, ok := s.pending[state]; ok {
		signin.saved = at
		s.pending[state] = signin
	}
}

// SigninStart is what the person needs to sign in.
type SigninStart struct {
	// State names this sign-in in the calls that follow.
	State string `json:"state"`
	// URL is the provider's page to open.
	URL string `json:"url"`
	// Flow is "paste" when the person brings back the address their browser ends at, and
	// "code" when the provider's page asks for UserCode.
	Flow      string `json:"flow"`
	UserCode  string `json:"user_code,omitempty"`
	ExpiresIn int    `json:"expires_in,omitempty"`
}

var (
	errUnknownProvider = errors.New("provider has to be claude, codex, xai or antigravity")
	errUnknownSignin   = errors.New("there is no such sign-in, or it has run out")
)

// startSignin begins a sign-in for a new subscription that will get rule, or, with
// subscription set, signs an existing one in again and leaves its rule as it is.
func (p *Pool) startSignin(ctx context.Context, provider, subscription string, rule Rule) (SigninStart, error) {
	if !signinProviders[provider] {
		return SigninStart{}, errUnknownProvider
	}
	body := map[string]any{}
	if subscription != "" {
		body["name"] = subscription
	}
	var answer struct {
		URL       string `json:"url"`
		State     string `json:"state"`
		Flow      string `json:"flow"`
		UserCode  string `json:"user_code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := p.management(ctx, http.MethodPost, "/oauth/auth-url?provider="+url.QueryEscape(provider), body, &answer); err != nil {
		return SigninStart{}, err
	}
	if answer.State == "" || answer.URL == "" {
		return SigninStart{}, errText("the proxy started no sign-in")
	}
	if subscription != "" {
		rule = p.state.rule(subscription)
	}
	had := map[string]bool{}
	for _, auth := range p.manager.List() {
		if auth != nil && strings.EqualFold(auth.Provider, provider) {
			had[auth.ID] = true
		}
	}
	p.signins.add(answer.State, pendingSignin{provider: provider, rule: rule, started: p.now(), subscription: subscription, had: had})
	start := SigninStart{State: answer.State, URL: answer.URL, Flow: "paste", ExpiresIn: answer.ExpiresIn}
	if answer.Flow == "device" {
		start.Flow, start.UserCode = "code", answer.UserCode
	}
	return start, nil
}

// signinCallback hands the proxy the address the person's browser ended at.
func (p *Pool) signinCallback(ctx context.Context, state, redirectURL string) error {
	if _, ok := p.signins.get(state); !ok {
		return errUnknownSignin
	}
	parsed, err := url.Parse(redirectURL)
	if err != nil || parsed.Query().Get("code") == "" {
		return badRequest("that is not the address the sign-in ended at: it has no code in it")
	}
	if parsed.Query().Get("state") != state {
		return badRequest("that address belongs to another sign-in")
	}
	return p.management(ctx, http.MethodPost, "/oauth/callback", map[string]string{"redirect_url": redirectURL}, nil)
}

// SigninStatus is how far a sign-in is.
type SigninStatus struct {
	// Status is "wait", "ok" or "error".
	Status string `json:"status"`
	// Subscription is the id of the account, once Status is "ok".
	Subscription string `json:"subscription,omitempty"`
	Error        string `json:"error,omitempty"`
}

// signinStatus asks the proxy how far a sign-in is. When it is done, the new account gets
// the rule the sign-in was started with, and the sign-in is forgotten.
func (p *Pool) signinStatus(ctx context.Context, state string) (SigninStatus, error) {
	signin, ok := p.signins.get(state)
	if !ok {
		return SigninStatus{}, errUnknownSignin
	}
	// The proxy forgets a finished sign-in a minute after it saved the account, so once it has
	// said so, it is not asked again.
	if signin.saved.IsZero() {
		var answer struct {
			Status string `json:"status"`
			AuthID string `json:"auth_id"`
			Error  string `json:"error"`
		}
		if err := p.management(ctx, http.MethodGet, "/oauth/status?state="+url.QueryEscape(state), nil, &answer); err != nil {
			return SigninStatus{}, err
		}
		switch answer.Status {
		case "ok":
			if answer.AuthID != "" {
				return p.signedIn(state, signin, answer.AuthID)
			}
			signin.saved = p.now()
			p.signins.markSaved(state, signin.saved)
		case "error":
			p.signins.remove(state)
			return SigninStatus{Status: "error", Error: answer.Error}, nil
		default:
			return SigninStatus{Status: "wait"}, nil
		}
	}
	if signin.subscription != "" {
		return p.signedIn(state, signin, signin.subscription)
	}
	// The account may take a moment to join the pool after the proxy saved it.
	if id, ok := p.newAccount(signin); ok {
		return p.signedIn(state, signin, id)
	}
	if p.now().Sub(signin.saved) < signinSettle {
		return SigninStatus{Status: "wait"}, nil
	}
	p.signins.remove(state)
	return SigninStatus{Status: "error", Error: "the proxy saved the login, but the pool cannot tell which subscription it is: it may be in the pool already"}, nil
}

// newAccount is the account a new sign-in made: the one of its provider that was not there
// when the sign-in started. Two of them would leave it unknown which is this sign-in's.
func (p *Pool) newAccount(signin pendingSignin) (string, bool) {
	var found []string
	for _, auth := range p.manager.List() {
		if auth != nil && strings.EqualFold(auth.Provider, signin.provider) && !signin.had[auth.ID] {
			found = append(found, auth.ID)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// signedIn gives the account the sign-in's rule and forgets the sign-in.
func (p *Pool) signedIn(state string, signin pendingSignin, id string) (SigninStatus, error) {
	if err := p.state.setRule(id, signin.rule); err != nil {
		return SigninStatus{}, err
	}
	p.signins.remove(state)
	return SigninStatus{Status: "ok", Subscription: id}, nil
}

func (p *Pool) cancelSignin(ctx context.Context, state string) error {
	if _, ok := p.signins.get(state); !ok {
		return errUnknownSignin
	}
	p.signins.remove(state)
	return p.management(ctx, http.MethodDelete, "/oauth/session?state="+url.QueryEscape(state), nil, nil)
}
