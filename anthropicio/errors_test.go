package anthropicio

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
)

// anthErr builds a fully-populated *anthropic.Error so that its Error() method
// (which dereferences Request and Response) does not panic. The real SDK always
// sets these; a bare struct literal does not.
func anthErr(status int) *anthropic.Error {
	return &anthropic.Error{
		StatusCode: status,
		Request:    &http.Request{Method: "POST", URL: &url.URL{Scheme: "https", Host: "api", Path: "/v1/messages"}},
		Response:   &http.Response{StatusCode: status},
	}
}

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

func TestMapBackendError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantKind   string
		wantStatus int
	}{
		{"anthropic 429", anthErr(429), "rate_limit", 429},
		{"anthropic 529", anthErr(529), "overloaded", 503},
		{"unknown", errors.New("boom"), "upstream", 502},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ae := MapBackendError(c.err)
			if ae.Kind != c.wantKind {
				t.Errorf("Kind = %q, want %q", ae.Kind, c.wantKind)
			}
			// MapError on the returned APIError must yield the expected HTTP status.
			status, _ := MapError(ae)
			if status != c.wantStatus {
				t.Errorf("MapError status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}
