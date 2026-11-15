package pool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TokenPrefix starts every token the pool makes. A key without it is not the pool's and is
// left to upstream's own key list.
const TokenPrefix = "m12_"

// Token is what the pool keeps of a token: whose it is, never the token itself.
type Token struct {
	// Person is the id the site gave for the token's owner.
	Person string `json:"person"`
	// Name tells a person's tokens apart, such as "dashboard" or "t3code".
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

type stateFile struct {
	Version int `json:"version"`
	// Operator is the person behind the keys in upstream's own key list.
	Operator string `json:"operator,omitempty"`
	// Tokens by the SHA-256 of the token, in hex.
	Tokens map[string]Token `json:"tokens"`
	// Subscriptions by account id, which is the name of its file.
	Subscriptions map[string]Rule `json:"subscriptions"`
}

// State is the pool's tokens and rules, kept in memory and saved to state.json.
type State struct {
	mu    sync.RWMutex
	path  string
	data  stateFile
	dirty bool
}

func loadState(dir string) (*State, error) {
	s := &State{path: filepath.Join(dir, "state.json")}
	s.data = stateFile{Version: 1, Tokens: map[string]Token{}, Subscriptions: map[string]Rule{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", s.path, err)
	}
	if s.data.Version != 1 {
		return nil, fmt.Errorf("%s has version %d, this build reads version 1", s.path, s.data.Version)
	}
	if s.data.Tokens == nil {
		s.data.Tokens = map[string]Token{}
	}
	if s.data.Subscriptions == nil {
		s.data.Subscriptions = map[string]Rule{}
	}
	return s, nil
}

// writeFileAtomic replaces path with data, so a crash leaves the old file or the new one.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *State) saveIfDirty() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, raw); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

// save writes now. The site API uses it, so a change it was told is saved is saved.
func (s *State) save() error {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
	return s.saveIfDirty()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// mintToken makes a token for a person, replacing the one of the same name they had.
// The token is returned once and is not kept.
func (s *State) mintToken(person, name string, now time.Time) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(random)
	s.mu.Lock()
	for hash, existing := range s.data.Tokens {
		if existing.Person == person && existing.Name == name {
			delete(s.data.Tokens, hash)
		}
	}
	s.data.Tokens[hashToken(token)] = Token{Person: person, Name: name, CreatedAt: now.UTC()}
	s.mu.Unlock()
	return token, s.save()
}

// revokeTokens removes a person's token of that name, or all of theirs when name is empty.
func (s *State) revokeTokens(person, name string) (int, error) {
	removed := 0
	s.mu.Lock()
	for hash, existing := range s.data.Tokens {
		if existing.Person == person && (name == "" || existing.Name == name) {
			delete(s.data.Tokens, hash)
			removed++
		}
	}
	s.mu.Unlock()
	if removed == 0 {
		return 0, nil
	}
	return removed, s.save()
}

// lookupToken tells whose a token is, and notes that it was used.
func (s *State) lookupToken(token string, now time.Time) (Token, bool) {
	hash := hashToken(token)
	s.mu.RLock()
	found, ok := s.data.Tokens[hash]
	s.mu.RUnlock()
	if !ok {
		return Token{}, false
	}
	// Once a minute is enough to show when a token was last used.
	if now.Sub(found.LastUsedAt) > time.Minute {
		s.mu.Lock()
		if current, still := s.data.Tokens[hash]; still {
			current.LastUsedAt = now.UTC()
			s.data.Tokens[hash] = current
			s.dirty = true
		}
		s.mu.Unlock()
	}
	return found, true
}

func (s *State) tokens() []Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Token, 0, len(s.data.Tokens))
	for _, token := range s.data.Tokens {
		out = append(out, token)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Person != out[j].Person {
			return out[i].Person < out[j].Person
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *State) tokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Tokens)
}

func (s *State) ruleCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Subscriptions)
}

func (s *State) operator() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Operator
}

func (s *State) setOperator(person string) error {
	s.mu.Lock()
	s.data.Operator = person
	s.mu.Unlock()
	return s.save()
}

// rule returns the rule of a subscription. One without a rule is open to everyone.
func (s *State) rule(subscription string) Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Subscriptions[subscription]
}

func (s *State) setRule(subscription string, rule Rule) error {
	s.mu.Lock()
	s.data.Subscriptions[subscription] = rule
	s.mu.Unlock()
	return s.save()
}

func (s *State) removeRule(subscription string) error {
	s.mu.Lock()
	_, had := s.data.Subscriptions[subscription]
	delete(s.data.Subscriptions, subscription)
	s.mu.Unlock()
	if !had {
		return nil
	}
	return s.save()
}
