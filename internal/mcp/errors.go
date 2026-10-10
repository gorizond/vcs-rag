package mcp

import (
	"context"
	"errors"
)

// Коды структурированных ошибок инструментов (contracts/mcp-tools.md:
// при ошибке — {"error": {...}}; сервер продолжает работу).
const (
	codeTimeout      = "timeout"
	codeCanceled     = "canceled"
	codeEmbedFailed  = "embedding_unavailable"
	codeSearchFailed = "qdrant_unavailable"
)

// toolError — структурированная ошибка инструмента.
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorResult классифицирует ошибку бэкенда: таймаут/отмена контекста
// важнее специфичного кода бэкенда; сообщение — без секретов.
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
