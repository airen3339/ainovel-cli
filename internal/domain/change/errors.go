package change

import "errors"

var (
	ErrUnauthorized        = errors.New("change is not authorized")
	ErrSemanticUnavailable = errors.New("semantic analysis is unavailable")
	ErrBasisMismatch       = errors.New("evidence basis no longer holds")
)
