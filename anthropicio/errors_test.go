package anthropicio

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMapErrorKinds(t *testing.T) {
	cases := []struct {
		err    error
		status int
		typ    string
	}{
		{NewAPIError("invalid_request", "bad"), 400, "invalid_request_error"},
		{NewAPIError("rate_limit", "slow down"), 429, "rate_limit_error"},
		{NewAPIError("overloaded", "busy"), 503, "overloaded_error"},
		{NewAPIError("upstream", "boom"), 502, "api_error"},
		{errors.New("generic"), 500, "api_error"},
	}
	for _, c := range cases {
		status, body := MapError(c.err)
		if status != c.status {
			t.Errorf("status for %v = %d, want %d", c.err, status, c.status)
		}
		var parsed struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if parsed.Type != "error" {
			t.Errorf("top type = %q", parsed.Type)
		}
		if parsed.Error.Type != c.typ {
			t.Errorf("error.type = %q, want %q", parsed.Error.Type, c.typ)
		}
		if parsed.Error.Message == "" {
			t.Errorf("empty message for %v", c.err)
		}
	}
}
