package pool

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// The site API is how a website runs the pool: it makes tokens for people, sets the rules of
// subscriptions, signs new subscriptions in and reads the usage figures. It listens on a port
// of its own and takes one key, the site's. It is not the management API: it cannot read an
// account's login, change the proxy's configuration or call a provider with an account.
//
// The site decides who may do what (that a rule is changed only by the subscription's
// owner, for example). The pool does what the site's key asks.
//
// docs/site-api.md in the fork describes every call.

func (p *Pool) serveAPI(ctx context.Context) error {
	if p.cfg.SiteKeyFile == "" {
		return errors.New("POOL_SITE_KEY_FILE is not set, and the site API does not run without a key")
	}
	if _, err := p.siteKey(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", p.cfg.Listen)
	if err != nil {
		return fmt.Errorf("the site API cannot listen on %s: %w", p.cfg.Listen, err)
	}
	server := &http.Server{Handler: p.apiHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("pool: the site API stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(stop)
	}()
	log.Infof("pool: site API on %s", listener.Addr())
	return nil
}

func (p *Pool) siteKey() (string, error) {
	raw, err := os.ReadFile(p.cfg.SiteKeyFile)
	if err != nil {
		return "", fmt.Errorf("the site key cannot be read: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if len(key) < 32 {
		return "", errors.New("the site key has to be at least 32 characters")
	}
	return key, nil
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(message string) error { return &apiError{http.StatusBadRequest, message} }
func notFound(message string) error   { return &apiError{http.StatusNotFound, message} }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	var api *apiError
	var management *managementError
	switch {
	case errors.As(err, &api):
		writeJSON(w, api.status, map[string]string{"error": api.message})
	case errors.Is(err, errUnknownSignin):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errUnknownProvider):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.As(err, &management) && management.Status >= 400 && management.Status < 500:
		// What the proxy refused about a sign-in is the person's to fix.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Errorf("pool: site API: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

func readJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return badRequest("the request body is not the JSON this call takes: " + err.Error())
	}
	return nil
}

// A person's id, or a token's name: what the site chooses, kept short and plain so it is
// safe in a file and in a log.
func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

func (p *Pool) apiHandler() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, fn func(r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			value, err := fn(r)
			if err != nil {
				writeError(w, err)
				return
			}
			if value == nil {
				value = map[string]string{"status": "ok"}
			}
			writeJSON(w, http.StatusOK, value)
		})
	}

	handle("GET /pool/v1/subscriptions", p.apiSubscriptions)
	handle("PUT /pool/v1/subscriptions/{id}/rule", p.apiSetRule)
	handle("DELETE /pool/v1/subscriptions/{id}", p.apiRemoveSubscription)
	handle("GET /pool/v1/tokens", p.apiTokens)
	handle("PUT /pool/v1/tokens/{person}/{name}", p.apiMintToken)
	handle("DELETE /pool/v1/tokens/{person}/{name}", p.apiRevokeToken)
	handle("DELETE /pool/v1/tokens/{person}", p.apiRevokeToken)
	handle("GET /pool/v1/operator", func(*http.Request) (any, error) {
		return map[string]string{"person": p.state.operator()}, nil
	})
	handle("PUT /pool/v1/operator", p.apiSetOperator)
	handle("GET /pool/v1/usage", func(*http.Request) (any, error) {
		return map[string]any{"week": WeekOf(p.now()), "rows": p.usage.Rows()}, nil
	})
	handle("GET /pool/v1/models", p.apiModels)
	handle("POST /pool/v1/signins", p.apiStartSignin)
	handle("GET /pool/v1/signins/{state}", func(r *http.Request) (any, error) {
		return p.signinStatus(r.Context(), r.PathValue("state"))
	})
	handle("POST /pool/v1/signins/{state}/callback", p.apiSigninCallback)
	handle("DELETE /pool/v1/signins/{state}", func(r *http.Request) (any, error) {
		return nil, p.cancelSignin(r.Context(), r.PathValue("state"))
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pool/v1/health" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		key, err := p.siteKey()
		if err != nil {
			log.Errorf("pool: site API: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the site key is not readable"})
			return
		}
		given := bearer(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(given), []byte(key)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this call needs the site key"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Subscription is an account of the pool as the site sees it.
type Subscription struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	// Kind is "oauth" for a signed-in subscription and "api_key" for a provider key.
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	Email string `json:"email,omitempty"`
	// Disabled accounts serve nothing. Status and StatusMessage are the proxy's own words
	// for the account's state, such as "active" or "error".
	Disabled      bool   `json:"disabled"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message,omitempty"`
	Weekly        struct {
		// Used is the share of the week's limit that is used, 0 to 1. It is only meaningful
		// with UsedKnown: Grok reports none.
		Used      float64    `json:"used"`
		UsedKnown bool       `json:"used_known"`
		ResetAt   *time.Time `json:"reset_at"`
	} `json:"weekly"`
	Rule Rule `json:"rule"`
}

func accountEmail(auth *coreauth.Auth) string {
	if email, ok := auth.Metadata["email"].(string); ok && strings.Contains(email, "@") {
		return email
	}
	if _, account := auth.AccountInfo(); strings.Contains(account, "@") {
		return account
	}
	return ""
}

func (p *Pool) subscriptions() []Subscription {
	now := p.now()
	auths := p.manager.List()
	out := make([]Subscription, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		sub := Subscription{
			ID:            auth.ID,
			Provider:      auth.Provider,
			Kind:          auth.AuthKind(),
			Label:         auth.Label,
			Email:         accountEmail(auth),
			Disabled:      auth.Disabled,
			Status:        string(auth.Status),
			StatusMessage: auth.StatusMessage,
			Rule:          p.state.rule(auth.ID),
		}
		week := p.weeklyOf(auth, now)
		sub.Weekly.Used, sub.Weekly.UsedKnown = week.Used, week.UsedKnown
		if !week.ResetAt.IsZero() {
			at := week.ResetAt.UTC()
			sub.Weekly.ResetAt = &at
		}
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *Pool) apiSubscriptions(*http.Request) (any, error) {
	return map[string]any{"subscriptions": p.subscriptions()}, nil
}

func (p *Pool) knownSubscription(id string) error {
	if _, ok := p.manager.GetByID(id); !ok {
		return notFound("there is no such subscription")
	}
	return nil
}

func (p *Pool) apiSetRule(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if err := p.knownSubscription(id); err != nil {
		return nil, err
	}
	var rule Rule
	if err := readJSON(r, &rule); err != nil {
		return nil, err
	}
	if err := rule.validate(); err != nil {
		return nil, badRequest(err.Error())
	}
	if err := p.state.setRule(id, rule); err != nil {
		return nil, err
	}
	return rule, nil
}

// apiRemoveSubscription takes an account out of the proxy, login and all.
func (p *Pool) apiRemoveSubscription(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if err := p.knownSubscription(id); err != nil {
		return nil, err
	}
	if err := p.management(r.Context(), http.MethodDelete, "/credentials?name="+url.QueryEscape(id), nil, nil); err != nil {
		return nil, err
	}
	return nil, p.state.removeRule(id)
}

func (p *Pool) apiTokens(*http.Request) (any, error) {
	return map[string]any{"tokens": p.state.tokens()}, nil
}

func (p *Pool) apiMintToken(r *http.Request) (any, error) {
	person, name := r.PathValue("person"), r.PathValue("name")
	if !validName(person) || !validName(name) {
		return nil, badRequest("a person's id and a token's name are 1 to 128 letters, digits, '_', '-' and '.'")
	}
	token, err := p.state.mintToken(person, name, p.now())
	if err != nil {
		return nil, err
	}
	return map[string]string{"token": token}, nil
}

func (p *Pool) apiRevokeToken(r *http.Request) (any, error) {
	removed, err := p.state.revokeTokens(r.PathValue("person"), r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	return map[string]int{"removed": removed}, nil
}

func (p *Pool) apiSetOperator(r *http.Request) (any, error) {
	var body struct {
		Person string `json:"person"`
	}
	if err := readJSON(r, &body); err != nil {
		return nil, err
	}
	if body.Person != "" && !validName(body.Person) {
		return nil, badRequest("that is not a person's id")
	}
	return nil, p.state.setOperator(body.Person)
}

// apiModels lists the models the pool's accounts serve right now. The proxy refreshes its
// list by itself, so a model a provider releases shows here without anything being released.
func (p *Pool) apiModels(*http.Request) (any, error) {
	type model struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by,omitempty"`
	}
	seen := map[string]bool{}
	models := []model{}
	for _, entry := range registry.GetGlobalRegistry().GetAvailableModels("openai") {
		id, _ := entry["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		owner, _ := entry["owned_by"].(string)
		models = append(models, model{ID: id, OwnedBy: owner})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return map[string]any{"models": models}, nil
}

func (p *Pool) apiStartSignin(r *http.Request) (any, error) {
	var body struct {
		Provider string `json:"provider"`
		// Subscription, when set, is an account to sign in again. Rule is then ignored.
		Subscription string `json:"subscription"`
		Rule         Rule   `json:"rule"`
	}
	if err := readJSON(r, &body); err != nil {
		return nil, err
	}
	if body.Subscription != "" {
		if err := p.knownSubscription(body.Subscription); err != nil {
			return nil, err
		}
	} else if err := body.Rule.validate(); err != nil {
		return nil, badRequest(err.Error())
	}
	return p.startSignin(r.Context(), body.Provider, body.Subscription, body.Rule)
}

func (p *Pool) apiSigninCallback(r *http.Request) (any, error) {
	var body struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := readJSON(r, &body); err != nil {
		return nil, err
	}
	return nil, p.signinCallback(r.Context(), r.PathValue("state"), strings.TrimSpace(body.RedirectURL))
}
