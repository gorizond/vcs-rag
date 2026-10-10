package mcp

import (
	"context"
	"errors"
)

// Structured tool error codes (contracts/mcp-tools.md:
// on error — {"error": {...}}; the server keeps running).
const (
	codeTimeout      = "timeout"
	codeCanceled     = "canceled"
	codeEmbedFailed  = "embedding_unavailable"
	codeSearchFailed = "qdrant_unavailable"
)

// toolError — a structured tool error.
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorResult classifies a backend error: context timeout/cancel
// outranks the specific backend code; the message carries no secrets.
func errorResult(ctx context.Context, backendCode string, err error) toolError {
	code := backendCode
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = codeTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		code = codeCanceled
	}
	return toolError{Code: code, Message: err.Error()}
}
