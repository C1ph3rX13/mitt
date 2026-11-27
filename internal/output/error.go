package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Error codes are the stable SCREAMING_SNAKE identifiers exposed to callers.
const (
	CodeInvalidRequest     = "INVALID_REQUEST"
	CodeInvalidPatch       = "INVALID_PATCH"
	CodeBaseConflict       = "BASE_CONFLICT"
	CodeEntryNotFound      = "ENTRY_NOT_FOUND"
	CodeNoMatch            = "NO_MATCH"
	CodeNotRunning         = "NOT_RUNNING"
	CodeLocked             = "LOCKED"
	CodeAddrInUse          = "ADDR_IN_USE"
	CodeUpstreamTLS        = "UPSTREAM_TLS"
	CodeCAMissing          = "CA_MISSING"
	CodeCertInstallFailed  = "CERT_INSTALL_FAILED"
	CodeSkillExists        = "SKILL_EXISTS"
	CodeSkillInstallFailed = "SKILL_INSTALL_FAILED"
	CodeScopeDenied        = "SCOPE_DENIED"
	CodeClientCertRejected = "CLIENT_CERT_REJECTED"
	CodeTempFailure        = "TEMP_FAILURE"
	CodeInternalError      = "INTERNAL_ERROR"
)

// CodeInfo describes one error code in the machine-readable command contract.
type CodeInfo struct {
	Code        string `json:"code"`
	ExitCode    int    `json:"exit_code"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

// ExitCodeInfo describes one process exit code.
type ExitCodeInfo struct {
	Code        int    `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

var codeTable = []CodeInfo{
	{Code: CodeInvalidRequest, ExitCode: 2, Description: "The command line or request is invalid."},
	{Code: CodeInvalidPatch, ExitCode: 2, Description: "The JSON Merge Patch could not be parsed or applied."},
	{Code: CodeBaseConflict, ExitCode: 2, Description: "Exactly one send base must be provided."},
	{Code: CodeEntryNotFound, ExitCode: 66, Description: "The referenced history entry does not exist."},
	{Code: CodeNoMatch, ExitCode: 66, Description: "No history entries matched the query."},
	{Code: CodeNotRunning, ExitCode: 69, Retryable: true, Description: "The proxy is not running."},
	{Code: CodeLocked, ExitCode: 69, Retryable: true, Description: "Another proxy is already using this data directory."},
	{Code: CodeAddrInUse, ExitCode: 69, Description: "The requested listen address is already in use."},
	{Code: CodeUpstreamTLS, ExitCode: 1, Description: "TLS handshake with the upstream failed."},
	{Code: CodeCAMissing, ExitCode: 78, Description: "The CA does not exist or is unreadable."},
	{Code: CodeCertInstallFailed, ExitCode: 78, Description: "Installing the CA into the trust store failed."},
	{Code: CodeSkillExists, ExitCode: 73, Description: "The destination skill already exists with different content."},
	{Code: CodeSkillInstallFailed, ExitCode: 78, Description: "Installing the bundled skill files failed."},
	{Code: CodeScopeDenied, ExitCode: 77, Description: "The target is outside the configured scope."},
	{Code: CodeClientCertRejected, ExitCode: 77, Description: "The client rejected the MITM certificate."},
	{Code: CodeTempFailure, ExitCode: 75, Retryable: true, Description: "A temporary failure occurred; retrying may succeed."},
	{Code: CodeInternalError, ExitCode: 1, Description: "An unexpected internal error occurred."},
}

var exitCodeTable = []ExitCodeInfo{
	{Code: 0, Name: "SUCCESS", Description: "Command completed successfully."},
	{Code: 1, Name: "ERROR", Description: "Generic error, including UPSTREAM_TLS."},
	{Code: 2, Name: "USAGE", Description: "Usage or argument error."},
	{Code: 66, Name: "NO_INPUT", Description: "Empty input, such as a missing entry or no matches."},
	{Code: 69, Name: "UNAVAILABLE", Description: "A required service is unavailable."},
	{Code: 73, Name: "CANT_CREATE", Description: "The destination already exists with conflicting content."},
	{Code: 75, Name: "TEMP_FAILURE", Description: "Temporary failure; safe to retry."},
	{Code: 77, Name: "NO_PERMISSION", Description: "Permission or scope denial."},
	{Code: 78, Name: "CONFIG", Description: "Configuration error."},
	{Code: 130, Name: "INTERRUPTED", Description: "Interrupted by SIGINT."},
}

// Codes returns the error code registry in stable order.
func Codes() []CodeInfo {
	out := make([]CodeInfo, len(codeTable))
	copy(out, codeTable)
	return out
}

// ExitCodes returns the exit code registry in stable order.
func ExitCodes() []ExitCodeInfo {
	out := make([]ExitCodeInfo, len(exitCodeTable))
	copy(out, exitCodeTable)
	return out
}

// LookupCode returns the registry entry for code.
func LookupCode(code string) (CodeInfo, bool) {
	for _, info := range codeTable {
		if info.Code == code {
			return info, true
		}
	}
	return CodeInfo{}, false
}

// AppError is an error with a stable code, an actionable hint, and details.
type AppError struct {
	Code    string
	Message string
	Hint    string
	Details map[string]any
}

// NewError builds an application error. Unknown codes fall back to
// INTERNAL_ERROR so callers can never emit an unregistered code.
func NewError(code, message, hint string) *AppError {
	if _, ok := LookupCode(code); !ok {
		code = CodeInternalError
	}
	return &AppError{
		Code:    code,
		Message: message,
		Hint:    hint,
		Details: map[string]any{},
	}
}

// WithDetail attaches one detail value and returns the error for chaining.
func (e *AppError) WithDetail(key string, value any) *AppError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func (e *AppError) Error() string {
	return e.Message
}

// ExitCode is the process exit code for this error.
func (e *AppError) ExitCode() int {
	info, ok := LookupCode(e.Code)
	if !ok {
		return 1
	}
	return info.ExitCode
}

// Retryable reports whether retrying the same command may succeed.
func (e *AppError) Retryable() bool {
	info, ok := LookupCode(e.Code)
	return ok && info.Retryable
}

// AsError converts any error to an AppError, treating unrecognized errors as
// INTERNAL_ERROR. The CLI layer classifies command-line parsing failures as
// INVALID_REQUEST before calling WriteError.
func AsError(err error) *AppError {
	var appErr *AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return NewError(CodeInternalError, err.Error(), "re-run with valid arguments; if the problem persists, report it")
}

type errorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Hint      string         `json:"hint"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// WriteError writes the stable error object to w. Error output is always JSON,
// regardless of the requested human-readable format.
func WriteError(w io.Writer, err error) error {
	appErr := AsError(err)
	details := appErr.Details
	if details == nil {
		details = map[string]any{}
	}
	envelope := errorEnvelope{
		Error: errorBody{
			Code:      appErr.Code,
			Message:   appErr.Message,
			Hint:      appErr.Hint,
			Retryable: appErr.Retryable(),
			Details:   details,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(envelope)
}

// UsageError builds an INVALID_REQUEST error for command-line usage mistakes.
func UsageError(message string) *AppError {
	return NewError(CodeInvalidRequest, message, "run `mitt --help` to see valid commands and flags")
}

// InternalErrorf builds an internal error with a formatted message.
func InternalErrorf(format string, args ...any) *AppError {
	return NewError(CodeInternalError, fmt.Sprintf(format, args...), "re-run the command; if the problem persists, report it")
}
