package pool

import (
	"context"
	"net/http"
	"strings"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// accessProviderName is what the proxy records as the way a request signed in.
	accessProviderName = "pool"
	// principalPrefix starts the name the proxy knows a person's requests by. It stands in
	// for the token everywhere the proxy would otherwise keep the key itself.
	principalPrefix = "pool:"
)

// accessProvider signs a request in by one of the pool's tokens.
type accessProvider struct{}

func (accessProvider) Identifier() string { return accessProviderName }

func (accessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	candidates := requestKeys(r)
	if len(candidates) == 0 {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	ours := false
	p := active()
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate.value, TokenPrefix) {
			continue
		}
		ours = true
		if p == nil {
			continue
		}
		if token, ok := p.state.lookupToken(candidate.value, p.now()); ok {
			return &sdkaccess.Result{
				Provider:  accessProviderName,
				Principal: principalPrefix + token.Person,
				Metadata:  map[string]string{"source": candidate.source, "token": token.Name},
			}, nil
		}
	}
	if ours {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	// Not one of the pool's tokens: upstream's own key list decides.
	return nil, sdkaccess.NewNotHandledError()
}

type requestKey struct{ value, source string }

// requestKeys finds a key in the places upstream's own key check looks.
func requestKeys(r *http.Request) []requestKey {
	if r == nil {
		return nil
	}
	found := []requestKey{
		{bearer(r.Header.Get("Authorization")), "authorization"},
		{r.Header.Get("X-Goog-Api-Key"), "x-goog-api-key"},
		{r.Header.Get("X-Api-Key"), "x-api-key"},
	}
	if r.URL != nil {
		query := r.URL.Query()
		found = append(found, requestKey{query.Get("key"), "query-key"}, requestKey{query.Get("auth_token"), "query-auth-token"})
	}
	keys := found[:0]
	for _, key := range found {
		key.value = strings.TrimSpace(key.value)
		if key.value != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func bearer(header string) string {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return header
	}
	if !strings.EqualFold(scheme, "bearer") {
		return header
	}
	return strings.TrimSpace(rest)
}

// Who a request is from, as far as the pool is concerned.
type caller struct {
	// person is the id of the person, empty for someone the pool does not know.
	person string
	// internal is a request the proxy makes itself, such as checking an account. The rules
	// are about people, so they do not apply to it.
	internal bool
}

// personOfPrincipal maps what the proxy recorded about a request's sign-in to a person.
// A key from upstream's own key list is the operator's.
func (p *Pool) personOfPrincipal(principal string) caller {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return caller{internal: true}
	}
	if person, ok := strings.CutPrefix(principal, principalPrefix); ok {
		return caller{person: person}
	}
	return caller{person: p.state.operator()}
}

func (p *Pool) callerOf(ctx context.Context) caller {
	return p.personOfPrincipal(coreusage.APIKeyFromContext(ctx))
}
