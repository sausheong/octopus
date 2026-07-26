package anthropicio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// cancelWrapper mimics an SDK that wraps context cancellation inside its own
// error type. MapBackendError must detect the wrapped cancellation before it
// reaches the SDK type switches, or cancellation is misclassified as a
// retryable upstream failure and the fan-out defect persists.
type cancelWrapper struct{ inner error }

func (c cancelWrapper) Error() string { return "sdk: " + c.inner.Error() }
func (c cancelWrapper) Unwrap() error { return c.inner }

func TestMapBackendErrorClassifiesCancellation(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare canceled", context.Canceled},
		{"bare deadline", context.DeadlineExceeded},
		{"wrapped canceled", cancelWrapper{inner: context.Canceled}},
		{"wrapped in fmt.Errorf", fmt.Errorf("stream open: %w", context.Canceled)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapBackendError(c.err).Kind; got != KindCanceled {
				t.Errorf("Kind = %q, want %q", got, KindCanceled)
			}
		})
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", NewAPIError("rate_limit", "slow down"), true},
		{"overloaded", NewAPIError("overloaded", "busy"), true},
		{"upstream", NewAPIError("upstream", "boom"), true},
		{"invalid request", NewAPIError("invalid_request", "bad max_tokens"), false},
		{"canceled", NewAPIError(KindCanceled, "gone"), false},
		{"anthropic 429", anthErr(429), true},
		{"anthropic 400", anthErr(400), false},
		{"anthropic 529", anthErr(529), true},
		{"raw context cancel", context.Canceled, false},
		{"unknown error", errors.New("mystery"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Retryable(c.err); got != c.want {
				t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
