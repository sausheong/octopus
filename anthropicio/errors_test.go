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
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
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
		// 499, not 500: a client hang-up is not a server error.
		{NewAPIError(KindCanceled, "client went away"), 499, "invalid_request_error"},
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

// TestMapBackendErrorPerProviderStatuses pins the status classification for
// every provider SDK error type. A 400 must be terminal for all of them: a
// malformed request fails identically on the next backend, so retrying it wastes
// the attempt budget and masks the real error behind a generic 502.
func TestMapBackendErrorPerProviderStatuses(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantKind      string
		wantRetryable bool
	}{
		{"anthropic 400", anthErr(400), "invalid_request", false},
		{"anthropic 429", anthErr(429), "rate_limit", true},
		{"anthropic 500", anthErr(500), "overloaded", true},
		{"anthropic 529", anthErr(529), "overloaded", true},

		{"openai APIError 400", &openai.APIError{HTTPStatusCode: 400, Message: "bad"}, "invalid_request", false},
		{"openai APIError 429", &openai.APIError{HTTPStatusCode: 429, Message: "slow"}, "rate_limit", true},
		{"openai APIError 503", &openai.APIError{HTTPStatusCode: 503, Message: "busy"}, "overloaded", true},

		{"openai RequestError 400", &openai.RequestError{HTTPStatusCode: 400, Err: nil}, "invalid_request", false},
		{"openai RequestError 429", &openai.RequestError{HTTPStatusCode: 429, Err: nil}, "rate_limit", true},
		{"openai RequestError 503", &openai.RequestError{HTTPStatusCode: 503, Err: nil}, "overloaded", true},

		{"gemini 400", genai.APIError{Code: 400, Message: "bad"}, "invalid_request", false},
		{"gemini 429", genai.APIError{Code: 429, Message: "slow"}, "rate_limit", true},
		{"gemini 503", genai.APIError{Code: 503, Message: "busy"}, "overloaded", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MapBackendError(c.err).Kind; got != c.wantKind {
				t.Errorf("Kind = %q, want %q", got, c.wantKind)
			}
			if got := Retryable(c.err); got != c.wantRetryable {
				t.Errorf("Retryable = %v, want %v", got, c.wantRetryable)
			}
		})
	}
}

// cancelWrapper is a custom error type that unwraps to a cancellation but
// matches none of the SDK branches. It only proves errors.Is reaches through an
// opaque wrapper; it cannot test ordering, since with nothing to shadow it the
// cancellation check yields the same answer wherever it sits in the function.
// The shadowing cases below are what pin the ordering.
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
		// These two both wrap cancellation AND match a real SDK branch, so they
		// are only classified as canceled if that check runs before the SDK type
		// switches. Without them, moving the check lower still passes the suite.
		{"openai 500 wrapping cancel", &openai.RequestError{HTTPStatusCode: 500, Err: context.Canceled}},
		{"anthropic 529 joined with cancel", fmt.Errorf("%w: %w", anthErr(529), context.Canceled)},
		// Pins cancellation above the APIError passthrough specifically: swapping
		// those two would classify this as overloaded rather than canceled.
		{"classified overloaded wrapping cancel", fmt.Errorf("%w: %w", NewAPIError("overloaded", "busy"), context.Canceled)},
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
		// Guards against a panic, not a wrong answer: MapBackendError would
		// dereference nil, and the fallback loops consult this predicate on a
		// lastErr value that can still be nil.
		{"nil error", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Retryable(c.err); got != c.want {
				t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
