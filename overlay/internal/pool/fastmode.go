package pool

import (
	"context"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Fast mode is something the client asks for in the request:
//
//   - Claude:  "speed": "fast", with the beta "fast-mode-…" in the Anthropic-Beta header
//   - Codex:   "service_tier": "priority" ("fast" and "ultrafast" mean it too)
//
// Both sit at the top of the request as the client sent it, whichever of the proxy's
// endpoints it used, because the proxy translates between the two later. Grok has no such
// setting: its fast mode is a model of its own, which the scheduler refuses.

var fastServiceTiers = map[string]bool{"priority": true, "fast": true, "ultrafast": true}

const fastModeBetaPrefix = "fast-mode-"

// withoutFastMode returns the request without its fast-mode setting, and whether it had one.
func withoutFastMode(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	doc := ojson.NewDoc(body)
	changed := false
	if strings.EqualFold(doc.Get("speed").String(), "fast") {
		if doc.Delete("speed") == nil {
			changed = true
		}
	}
	if fastServiceTiers[strings.ToLower(doc.Get("service_tier").String())] {
		if doc.Delete("service_tier") == nil {
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	return doc.Bytes(), true
}

// withoutFastModeBeta drops the fast-mode beta from the Anthropic-Beta header and keeps the
// other betas. It returns nil when the header has no such beta.
func withoutFastModeBeta(headers http.Header) http.Header {
	values := headers.Values("Anthropic-Beta")
	found := false
	kept := make([]string, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ",")
		keep := parts[:0]
		for _, part := range parts {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(part)), fastModeBetaPrefix) {
				found = true
				continue
			}
			keep = append(keep, part)
		}
		if len(keep) > 0 {
			kept = append(kept, strings.Join(keep, ","))
		}
	}
	if !found {
		return nil
	}
	return http.Header{"Anthropic-Beta": kept}
}

// WrapAfterAuth adds the pool's step to what runs once an account is chosen for a request and
// before the request is translated for it: if that account's owner does not allow fast mode,
// the request goes on at normal speed. next is upstream's own step, nil for none.
func WrapAfterAuth(next cliproxyexecutor.RequestAfterAuthInterceptor) cliproxyexecutor.RequestAfterAuthInterceptor {
	if active() == nil {
		return next
	}
	return func(ctx context.Context, req cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
		var resp cliproxyexecutor.RequestAfterAuthInterceptResponse
		if next != nil {
			resp = next(ctx, req)
		}
		p := active()
		if p == nil || resp.Terminate {
			return resp
		}
		account, _ := req.Metadata[cliproxyexecutor.SelectedAuthMetadataKey].(string)
		if account == "" {
			return resp
		}
		who := p.callerOf(ctx)
		if who.internal || !p.state.rule(account).stripsFastMode(who.person) {
			return resp
		}
		body := req.Body
		if len(resp.Body) > 0 {
			body = resp.Body
		}
		if stripped, changed := withoutFastMode(body); changed {
			resp.Body = stripped
		}
		headers := req.Headers
		if resp.Headers != nil {
			if _, set := resp.Headers["Anthropic-Beta"]; set {
				headers = resp.Headers
			}
		}
		if cleaned := withoutFastModeBeta(headers); cleaned != nil {
			if resp.Headers == nil {
				resp.Headers = http.Header{}
			}
			if kept := cleaned["Anthropic-Beta"]; len(kept) > 0 {
				resp.Headers["Anthropic-Beta"] = kept
			} else {
				delete(resp.Headers, "Anthropic-Beta")
				resp.ClearHeaders = append(resp.ClearHeaders, "Anthropic-Beta")
			}
		}
		return resp
	}
}
