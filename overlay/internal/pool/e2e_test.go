//go:build poole2e

package pool

// This test runs the proxy itself, the built program, against a stand-in for Anthropic, and
// checks from the outside that the pool's three hooks into upstream's code are in place:
// who a request is from reaches the choice of account, the step after that choice changes
// the request, and finished requests are counted. A new upstream release that moves any of
// these fails here, before it is shipped.
//
//	go build -o /tmp/cli-proxy-api ./cmd/server
//	POOL_E2E_BINARY=/tmp/cli-proxy-api go test -tags poole2e -run TestEndToEnd ./internal/pool/

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const e2eModel = "claude-sonnet-4-5-20250929"

// upstreamCall is one request the stand-in for Anthropic received.
type upstreamCall struct {
	key  string
	body map[string]any
}

// fakeAnthropic answers /v1/messages for two accounts, told apart by their API key, each
// with its own weekly figures in the answer's headers.
type fakeAnthropic struct {
	mu     sync.Mutex
	calls  []upstreamCall
	weekly map[string][2]string // key -> utilization, reset (unix seconds)
}

func (f *fakeAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, upstreamCall{key: key, body: body})
	figures, known := f.weekly[key]
	f.mu.Unlock()
	if known {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Utilization", figures[0])
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", figures[1])
	}
	model, _ := body["model"].(string)
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","model":"` + model + `","content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":1}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}
		for _, event := range events {
			_, _ = io.WriteString(w, event+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"`+model+`","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":7}}`)
}

func (f *fakeAnthropic) last(t *testing.T) upstreamCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("the stand-in for Anthropic was never asked")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeAnthropic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

type e2e struct {
	t        *testing.T
	proxy    string
	site     string
	upstream *fakeAnthropic
}

func (e *e2e) do(base, key, method, path, body string) (int, map[string]any, string) {
	e.t.Helper()
	request, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded, string(raw)
}

func (e *e2e) siteCall(method, path, body string) map[string]any {
	e.t.Helper()
	status, decoded, raw := e.do(e.site, siteKey, method, path, body)
	if status != http.StatusOK {
		e.t.Fatalf("site API %s %s: %d %s", method, path, status, raw)
	}
	return decoded
}

func (e *e2e) token(person string) string {
	e.t.Helper()
	token, _ := e.siteCall("PUT", "/pool/v1/tokens/"+person+"/dashboard", "")["token"].(string)
	if token == "" {
		e.t.Fatalf("no token for %s", person)
	}
	return token
}

func (e *e2e) rule(subscription, rule string) {
	e.t.Helper()
	e.siteCall("PUT", "/pool/v1/subscriptions/"+subscription+"/rule", rule)
}

// message sends one request to the proxy's Anthropic endpoint, with extra as further fields.
func (e *e2e) message(key, extra string) (int, string) {
	e.t.Helper()
	body := `{"model":"` + e2eModel + `","max_tokens":16,"messages":[{"role":"user","content":"ping"}]` + extra + `}`
	status, _, raw := e.do(e.proxy, key, "POST", "/v1/messages", body)
	return status, raw
}

// servedBy sends a request that has to succeed and tells which account's key answered it.
func (e *e2e) servedBy(key, extra string) string {
	e.t.Helper()
	before := e.upstream.count()
	status, raw := e.message(key, extra)
	if status != http.StatusOK {
		e.t.Fatalf("a request that should be served: %d %s", status, raw)
	}
	if e.upstream.count() != before+1 {
		e.t.Fatalf("expected one request upstream, got %d", e.upstream.count()-before)
	}
	return e.upstream.last(e.t).key
}

func TestEndToEnd(t *testing.T) {
	binary := os.Getenv("POOL_E2E_BINARY")
	if binary == "" {
		t.Skip("POOL_E2E_BINARY is not set")
	}
	now := time.Now()
	upstream := &fakeAnthropic{weekly: map[string][2]string{
		"up-near": {"0.85", strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10)},
		"up-far":  {"0.10", strconv.FormatInt(now.Add(6*24*time.Hour).Unix(), 10)},
	}}
	fake := httptest.NewServer(upstream)
	defer fake.Close()

	dir := t.TempDir()
	proxyPort, sitePort := freePort(t), freePort(t)
	config := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
management:
  allow-remote: false
  secret-key: "e2e-management-key"
  disable-control-panel: true
access:
  api-keys:
    - "legacy-key"
oauth:
  auth-dir: %q
routing:
  strategy: "fill-first"
upstream:
  claude:
    disable-claude-cloak-mode: true
api-keys:
  claude:
    - name: near
      base-url: %q
      keys:
        - api-key: "up-near"
    - name: far
      base-url: %q
      keys:
        - api-key: "up-far"
`, proxyPort, filepath.Join(dir, "auth"), fake.URL, fake.URL)
	configPath := filepath.Join(dir, "config.yaml")
	for path, content := range map[string]string{
		configPath:                           config,
		filepath.Join(dir, "site-key"):       siteKey,
		filepath.Join(dir, "management-key"): "e2e-management-key",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	server := exec.Command(binary, "-config", configPath, "-local-model")
	server.Dir = dir
	server.Stdout, server.Stderr = &output, &output
	server.Env = append(os.Environ(),
		"POOL_STATE_DIR="+filepath.Join(dir, "pool"),
		"POOL_LISTEN=127.0.0.1:"+strconv.Itoa(sitePort),
		"POOL_SITE_KEY_FILE="+filepath.Join(dir, "site-key"),
		"POOL_MANAGEMENT_KEY_FILE="+filepath.Join(dir, "management-key"),
		"POOL_MANAGEMENT_URL=http://127.0.0.1:"+strconv.Itoa(proxyPort)+"/v8/management",
	)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = server.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = server.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = server.Process.Kill()
		}
		if t.Failed() {
			t.Logf("the proxy's output:\n%s", output.String())
		}
	}()

	e := &e2e{t: t, proxy: "http://127.0.0.1:" + strconv.Itoa(proxyPort), site: "http://127.0.0.1:" + strconv.Itoa(sitePort), upstream: upstream}
	deadline := time.Now().Add(60 * time.Second)
	for {
		ready := false
		if response, err := http.Get(e.site + "/pool/v1/health"); err == nil {
			response.Body.Close()
			// The site API can answer before the proxy listens. That is not up yet, not a failure.
			request, err := http.NewRequest(http.MethodGet, e.proxy+"/v1/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer legacy-key")
			if response, err := http.DefaultClient.Do(request); err == nil {
				response.Body.Close()
				ready = response.StatusCode == http.StatusOK
			}
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy did not come up:\n%s", output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The two accounts, by the key each sends upstream.
	accounts := map[string]string{}
	for _, raw := range e.siteCall("GET", "/pool/v1/subscriptions", "")["subscriptions"].([]any) {
		sub := raw.(map[string]any)
		if sub["provider"] != "claude" {
			continue
		}
		id := sub["id"].(string)
		e.rule(id, `{"owner":"nobody","share":"private"}`)
		for _, other := range e.siteCall("GET", "/pool/v1/subscriptions", "")["subscriptions"].([]any) {
			if otherID := other.(map[string]any)["id"].(string); otherID != id {
				e.rule(otherID, `{}`)
			}
		}
		// With this account closed to everyone, the other one answers.
		servedByOther := e.servedBy("legacy-key", "")
		for _, key := range []string{"up-near", "up-far"} {
			if key != servedByOther {
				accounts[key] = id
			}
		}
		e.rule(id, `{}`)
	}
	near, far := accounts["up-near"], accounts["up-far"]
	if near == "" || far == "" || near == far {
		t.Fatalf("could not tell the two accounts apart: %v", accounts)
	}

	ada, grace, linus := e.token("user_ada"), e.token("user_grace"), e.token("user_linus")

	t.Run("who may sign in", func(t *testing.T) {
		for name, key := range map[string]string{"no key": "", "a made-up token": TokenPrefix + "made-up", "a key of nobody": "wrong"} {
			if status, raw := e.message(key, ""); status != http.StatusUnauthorized {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		}
		for name, key := range map[string]string{"a key of the proxy's own list": "legacy-key", "a person's token": ada} {
			if status, raw := e.message(key, ""); status != http.StatusOK {
				t.Errorf("%s: %d %s", name, status, raw)
			}
		}
	})

	t.Run("the closest weekly reset is used first", func(t *testing.T) {
		// Both accounts have answered by now, so the proxy knows both resets.
		for name, key := range map[string]string{"ada": ada, "grace": grace} {
			if got := e.servedBy(key, ""); got != "up-near" {
				t.Errorf("%s was served by %s", name, got)
			}
		}
		subs := e.siteCall("GET", "/pool/v1/subscriptions", "")["subscriptions"].([]any)
		for _, raw := range subs {
			sub := raw.(map[string]any)
			weekly := sub["weekly"].(map[string]any)
			if sub["id"] == near && (weekly["used"] != 0.85 || weekly["reset_at"] == nil) {
				t.Errorf("the near account's weekly figures: %v", weekly)
			}
		}
	})

	t.Run("an owner's reserve sends the others on", func(t *testing.T) {
		e.rule(near, `{"owner":"user_grace","reserve_percent":20}`)
		defer e.rule(near, `{}`)
		if got := e.servedBy(ada, ""); got != "up-far" {
			t.Errorf("ada was served by %s", got)
		}
		if got := e.servedBy(grace, ""); got != "up-near" {
			t.Errorf("the owner was served by %s", got)
		}
		e.rule(far, `{"owner":"user_grace","share":"private"}`)
		defer e.rule(far, `{}`)
		status, raw := e.message(ada, "")
		if status != http.StatusTooManyRequests || !strings.Contains(raw, "share of this week") {
			t.Errorf("with nothing left for ada: %d %s", status, raw)
		}
	})

	t.Run("a subscription serves the people it is shared with", func(t *testing.T) {
		e.rule(near, `{"owner":"user_grace","share":"people","people":["user_ada"]}`)
		e.rule(far, `{"owner":"user_grace","share":"private"}`)
		defer e.rule(near, `{}`)
		defer e.rule(far, `{}`)
		if got := e.servedBy(ada, ""); got != "up-near" {
			t.Errorf("ada was served by %s", got)
		}
		if status, raw := e.message(linus, ""); status != http.StatusForbidden || !strings.Contains(raw, "shared with you") {
			t.Errorf("linus: %d %s", status, raw)
		}
	})

	t.Run("a blocked model", func(t *testing.T) {
		e.rule(near, `{"owner":"user_grace","blocked_models":["claude-sonnet-*"]}`)
		e.rule(far, `{"owner":"user_grace","blocked_models":["claude-sonnet-*"]}`)
		defer e.rule(near, `{}`)
		defer e.rule(far, `{}`)
		if status, raw := e.message(ada, ""); status != http.StatusForbidden || !strings.Contains(raw, "switched it off") {
			t.Errorf("ada: %d %s", status, raw)
		}
		if got := e.servedBy(grace, ""); got != "up-near" {
			t.Errorf("the owner was served by %s", got)
		}
	})

	t.Run("fast mode is taken off where the owner does not allow it", func(t *testing.T) {
		e.rule(near, `{"owner":"user_grace","block_fast_mode":true}`)
		defer e.rule(near, `{}`)
		if got := e.servedBy(ada, `,"speed":"fast"`); got != "up-near" {
			t.Fatalf("ada was served by %s", got)
		}
		if speed, has := upstream.last(t).body["speed"]; has {
			t.Errorf("ada's request went upstream with speed %v", speed)
		}
		e.servedBy(grace, `,"speed":"fast"`)
		if speed := upstream.last(t).body["speed"]; speed != "fast" {
			t.Errorf("the owner's request went upstream with speed %v", speed)
		}
		e.rule(near, `{"owner":"user_grace"}`)
		e.servedBy(ada, `,"speed":"fast"`)
		if speed := upstream.last(t).body["speed"]; speed != "fast" {
			t.Errorf("with fast mode allowed, ada's request went upstream with speed %v", speed)
		}
	})

	t.Run("a streamed answer comes through", func(t *testing.T) {
		status, raw := e.message(ada, `,"stream":true`)
		if status != http.StatusOK || !strings.Contains(raw, "pong") || !strings.Contains(raw, "message_stop") {
			t.Errorf("%d %s", status, raw)
		}
	})

	t.Run("what was used is counted for the person", func(t *testing.T) {
		// The proxy hands finished requests over a moment after it answers: wait until every
		// request that reached the stand-in is counted.
		var rows []any
		deadline := time.Now().Add(15 * time.Second)
		for {
			rows, _ = e.siteCall("GET", "/pool/v1/usage", "")["rows"].([]any)
			counted := 0.0
			for _, raw := range rows {
				counted += raw.(map[string]any)["requests"].(float64)
			}
			if int(counted) >= upstream.count() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%v of %d requests were counted: %v", counted, upstream.count(), rows)
			}
			time.Sleep(200 * time.Millisecond)
		}
		var adaInput, adaRequests float64
		people := map[string]bool{}
		for _, raw := range rows {
			row := raw.(map[string]any)
			people[row["person"].(string)] = true
			// linus was only ever refused, and a refusal reaches no subscription.
			if row["person"] == "user_linus" && row["subscription"] != "" {
				t.Errorf("a refused person has usage on a subscription: %v", row)
			}
			if row["person"] == "user_ada" && row["model"] == e2eModel {
				adaInput += row["input_tokens"].(float64)
				adaRequests += row["requests"].(float64)
				if row["subscription"] != near && row["subscription"] != far {
					t.Errorf("a row names an unknown subscription: %v", row)
				}
			}
		}
		if adaRequests < 5 || adaInput != adaRequests*11 {
			t.Errorf("ada: %v requests, %v input tokens; rows %v", adaRequests, adaInput, rows)
		}
		if !people["user_grace"] || !people[PersonUnknown] {
			t.Errorf("people in the figures: %v", people)
		}
	})

	t.Run("the pool's own files hold no token", func(t *testing.T) {
		// A save is due every five seconds.
		time.Sleep(6 * time.Second)
		for _, name := range []string{"state.json", "usage.json"} {
			raw, err := os.ReadFile(filepath.Join(dir, "pool", name))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, secret := range []string{ada, grace, linus, "legacy-key"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Errorf("%s holds a key in clear", name)
				}
			}
		}
	})
}
