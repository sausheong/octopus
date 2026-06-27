package anthropicio

import "encoding/json"

// APIError is a router error carrying a stable Kind that MapError translates
// into an HTTP status and Anthropic error type.
type APIError struct {
	Kind    string
	Message string
}

func (e APIError) Error() string { return e.Message }

// NewAPIError builds an APIError.
func NewAPIError(kind, msg string) APIError { return APIError{Kind: kind, Message: msg} }

// MapError returns the HTTP status and Anthropic-shaped error body for err.
func MapError(err error) (int, []byte) {
	status := 500
	errType := "api_error"
	msg := err.Error()

	if ae, ok := err.(APIError); ok {
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
