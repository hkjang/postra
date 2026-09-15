package domain

// ErrorResponse is the shared public REST/MCP failure contract. Details must
// contain only deliberately public fields, never upstream errors or secrets.
type ErrorResponse struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	TraceID string         `json:"trace_id"`
}

// PublicError is a classified, client-safe application failure.
type PublicError struct {
	Code    string
	Message string
	Details map[string]any
	Status  int
}

func (e *PublicError) Error() string { return e.Message }
