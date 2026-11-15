package pool

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// A sign-in adds someone's subscription to the pool without a shell on the proxy. The proxy's
// management API does the work; the pool starts it, passes on what the person brings back,
// and gives the new account its owner and rule.
//
//   - Claude and Codex: the provider's page ends at an address on the person's own computer
//     that nothing answers. They copy that address from their browser, and the site hands it
//     to SigninCallback.
//   - Grok: the provider's page asks for a code the proxy made. Nothing comes back by hand.

// Providers a subscription can be added for.
var signinProviders = map[string]bool{"claude": true, "codex": true, "xai": true}

// A sign-in that is not finished after this long is forgotten.
const signinLifetime = 20 * time.Minute

type pendingSignin struct {
	provider string
	rule     Rule
	started  time.Time
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
	errUnknownProvider = errors.New("provider has to be claude, codex or xai")
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
	p.signins.add(answer.State, pendingSignin{provider: provider, rule: rule, started: p.now()})
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
		if answer.AuthID == "" {
			return SigninStatus{}, errText("the proxy finished the sign-in without naming the account")
		}
		if err := p.state.setRule(answer.AuthID, signin.rule); err != nil {
			return SigninStatus{}, err
		}
		p.signins.remove(state)
		return SigninStatus{Status: "ok", Subscription: answer.AuthID}, nil
	case "error":
		p.signins.remove(state)
		return SigninStatus{Status: "error", Error: answer.Error}, nil
	default:
		return SigninStatus{Status: "wait"}, nil
	}
}

func (p *Pool) cancelSignin(ctx context.Context, state string) error {
	if _, ok := p.signins.get(state); !ok {
		return errUnknownSignin
	}
	p.signins.remove(state)
	return p.management(ctx, http.MethodDelete, "/oauth/session?state="+url.QueryEscape(state), nil, nil)
}
