package httpwire

import "errors"

// Errors returned by the wire parser and framing rules. Callers use errors.Is
// to distinguish failure modes while keeping their own context prefixes.
var (
	ErrLineTooLong                 = errors.New("HTTP line is too long")
	ErrHeadersTooLarge             = errors.New("HTTP headers are too large")
	ErrUpgradeUnsupported          = errors.New("protocol upgrades are not supported")
	ErrUnsupportedTransferEncoding = errors.New("unsupported Transfer-Encoding")
	ErrConflictingContentLength    = errors.New("conflicting Content-Length headers")
	ErrInvalidContentLength        = errors.New("invalid Content-Length")
)
