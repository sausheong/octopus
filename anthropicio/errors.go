package anthropicio

import (
	"context"
	"encoding/json"
	"errors"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
)

// APIError is a router error carrying a stable Kind that MapError translates
// into an HTTP status and Anthropic error type.
type APIError struct {
	Kind    string
	Message string
}

func (e APIError) Error() string { return e.Message }

// KindCanceled marks client-side cancellation: the caller went away, so there
// is no point trying another backend and no client left to write a response to.
const KindCanceled = "canceled"

// NewAPIError builds an APIError.
func NewAPIError(kind, msg string) APIError { return APIError{Kind: kind, Message: msg} }

// MapError returns the HTTP status and Anthropic-shaped error body for err.
func MapError(err error) (int, []byte) {
	status := 500
	errType := "api_error"
	msg := err.Error()

	var ae APIError
	if errors.As(err, &ae) {
		switch ae.Kind {
		case "invalid_request":
			status, errType = 400, "invalid_request_error"
		case "rate_limit":
			status, errType = 429, "rate_limit_error"
		case "overloaded":
			status, errType = 503, "overloaded_error"
		case "upstream":
			status, errType = 502, "api_error"
		}
	}

	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
	return status, body
}

// MapBackendError converts a harness provider error into an APIError with the
// right Kind, inspecting typed SDK errors via errors.As. Falls back to
// "upstream" (502) for unrecognized errors.
func MapBackendError(err error) APIError {
	// Checked first, before the SDK type switches: some SDKs wrap cancellation
	// inside their own error types, so an errors.As match on a provider error
	// would otherwise shadow it and misreport a hang-up as a retryable 502.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewAPIError(KindCanceled, err.Error())
	}
	// An already-classified error keeps its Kind: re-deriving it here would fall
	// through to "upstream" and turn a terminal failure back into a retryable one.
	var already APIError
	if errors.As(err, &already) {
		return already
	}
	// anthropic 429 -> rate_limit, 529 -> overloaded
	var anthErr *anthropic.Error
	if errors.As(err, &anthErr) {
		switch anthErr.StatusCode {
		case 429:
			return NewAPIError("rate_limit", err.Error())
		case 529:
			return NewAPIError("overloaded", err.Error())
		}
		if anthErr.StatusCode >= 500 {
			return NewAPIError("overloaded", err.Error())
		}
		if anthErr.StatusCode == 400 {
			return NewAPIError("invalid_request", err.Error())
		}
	}
	// openai APIError / RequestError: 429 -> rate_limit, 5xx -> overloaded
	var oaiAPI *openai.APIError
	if errors.As(err, &oaiAPI) {
		if oaiAPI.HTTPStatusCode == 429 {
			return NewAPIError("rate_limit", err.Error())
		}
		if oaiAPI.HTTPStatusCode >= 500 && oaiAPI.HTTPStatusCode < 600 {
			return NewAPIError("overloaded", err.Error())
		}
	}
	var oaiReq *openai.RequestError
	if errors.As(err, &oaiReq) {
		if oaiReq.HTTPStatusCode == 429 {
			return NewAPIError("rate_limit", err.Error())
		}
		if oaiReq.HTTPStatusCode >= 500 && oaiReq.HTTPStatusCode < 600 {
			return NewAPIError("overloaded", err.Error())
		}
	}
	// gemini: genai.APIError is a VALUE type (value receiver Error()), match the value
	var gErr genai.APIError
	if errors.As(err, &gErr) {
		if gErr.Code == 429 {
			return NewAPIError("rate_limit", err.Error())
		}
		if gErr.Code >= 500 && gErr.Code < 600 {
			return NewAPIError("overloaded", err.Error())
		}
	}
	return NewAPIError("upstream", err.Error())
}

// Retryable reports whether trying a different backend could plausibly help.
// A malformed request fails identically everywhere, and a cancelled request
// has no one waiting for it; everything else is worth another candidate.
func Retryable(err error) bool {
	switch MapBackendError(err).Kind {
	case "invalid_request", KindCanceled:
		return false
	default:
		return true
	}
}
