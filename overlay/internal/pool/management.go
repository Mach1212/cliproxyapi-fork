package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func errText(text string) error { return errors.New(text) }

func errStatus(what string, status int) error { return fmt.Errorf("%s answered HTTP %d", what, status) }

// managementError is an answer of the management API that is not a success.
type managementError struct {
	Status  int
	Message string
}

func (e *managementError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the management API answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("the management API answered HTTP %d: %s", e.Status, e.Message)
}

var managementClient = &http.Client{Timeout: 30 * time.Second}

// management calls upstream's management API over loopback with the management key. The
// pool uses it for the few things upstream already does well: signing an account in,
// removing one, and asking a provider something with an account's own token. The key never
// leaves this process, and the site API offers none of the management API's other calls.
func (p *Pool) management(ctx context.Context, method, path string, body, into any) error {
	if p.cfg.ManagementKeyFile == "" {
		return errText("POOL_MANAGEMENT_KEY_FILE is not set")
	}
	rawKey, err := os.ReadFile(p.cfg.ManagementKeyFile)
	if err != nil {
		return fmt.Errorf("the management key cannot be read: %w", err)
	}
	key := strings.TrimSpace(string(rawKey))
	if key == "" {
		return errText("the management key file is empty")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.cfg.ManagementURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := managementClient.Do(request)
	if err != nil {
		return fmt.Errorf("the management API does not answer: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("the management API's answer could not be read: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &failure)
		message := failure.Error
		if message == "" {
			message = failure.Message
		}
		return &managementError{Status: response.StatusCode, Message: message}
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("the management API's answer could not be read: %w", err)
	}
	return nil
}
