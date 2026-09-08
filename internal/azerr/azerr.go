// Package azerr reproduces the two *different* error contracts of the upstream Azure
// containers.
//
// Document Intelligence wraps: {"error":{"code","message","target","details","innererror"}}
// Computer Vision Read is flat: {"code","message","requestId"}
//
// Read is the odd one out even within Computer Vision — every other v3.2 operation uses the
// wrapped ComputerVisionErrorResponse from ComputerVision.json, but the Read routes are defined
// in Ocr.json and use the flat ComputerVisionOcrError. Writing this package from the wrong
// swagger file inverts the shape.
//
// Each surface's swagger declares exactly one `default` error response covering every non-2xx
// status, so the shape is chosen per surface, never per status code.
package azerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Surface selects which wire contract an error is rendered in.
type Surface int

const (
	// SurfaceDI is Document Intelligence: the wrapped shape.
	SurfaceDI Surface = iota
	// SurfaceRead is Computer Vision Read v3.2: the flat shape.
	SurfaceRead
)

func (s Surface) String() string {
	if s == SurfaceRead {
		return "read"
	}
	return "di"
}

// Top-level Document Intelligence error codes, with their canonical messages and HTTP statuses.
// Codes commonly mistaken for top-level — InvalidContent, InvalidContentDimensions,
// InvalidContentLength — are inner codes under InvalidRequest; see the Inner* constants.
const (
	CodeInvalidRequest       = "InvalidRequest"
	CodeInvalidArgument      = "InvalidArgument"
	CodeUnauthorized         = "Unauthorized"
	CodeForbidden            = "Forbidden"
	CodeNotFound             = "NotFound"
	CodeMethodNotAllowed     = "MethodNotAllowed"
	CodeConflict             = "Conflict"
	CodeUnsupportedMediaType = "UnsupportedMediaType"
	CodeInternalServerError  = "InternalServerError"
	CodeServiceUnavailable   = "ServiceUnavailable"
)

// Document Intelligence inner codes. These never appear at the top level.
const (
	InnerInvalidContent           = "InvalidContent"
	InnerInvalidContentLength     = "InvalidContentLength"
	InnerInvalidContentDimensions = "InvalidContentDimensions"
	InnerInvalidParameter         = "InvalidParameter"
	InnerParameterMissing         = "ParameterMissing"
	InnerNotSupportedAPIVersion   = "NotSupportedApiVersion"
	InnerOperationNotFound        = "OperationNotFound"
	InnerModelNotFound            = "ModelNotFound"
	InnerContentSourceNotAccess   = "ContentSourceNotAccessible"
	InnerContentSourceTimeout     = "ContentSourceTimeout"
	InnerUnknown                  = "Unknown"
)

// Computer Vision Read error codes — the complete 17-value ComputerVisionOcrErrorCodes enum.
// The enum is modelAsString:true, so unlisted values are legal, but these are the documented set.
// Note there is no not-found code in the Read enum at all.
const (
	ReadInvalidImageFormat    = "InvalidImageFormat"
	ReadUnsupportedMediaType  = "UnsupportedMediaType"
	ReadInvalidImageURL       = "InvalidImageUrl"
	ReadInternalServerError   = "InternalServerError"
	ReadInvalidImageSize      = "InvalidImageSize"
	ReadBadArgument           = "BadArgument"
	ReadNotSupportedLanguage  = "NotSupportedLanguage"
	ReadFailedToProcess       = "FailedToProcess"
	ReadUnspecified           = "Unspecified"
	ReadStorageException      = "StorageException"
	ReadInvalidPageRange      = "InvalidPageRange"
	ReadFailedToDownloadImage = "FailedToDownloadImage"
	ReadInvalidImage          = "InvalidImage"
	ReadUnsupportedImageFmt   = "UnsupportedImageFormat"
	ReadInvalidImageDimension = "InvalidImageDimension"
	ReadInvalidReadingOrder   = "InvalidReadingOrder"
	ReadInvalidRequest        = "InvalidRequest"
	// ReadUnauthorized is not in the enum, but container clients are known to branch on 401.
	ReadUnauthorized = "Unauthorized"
)

// canonicalMessages are the exact message strings Azure documents for each top-level DI code.
var canonicalMessages = map[string]string{
	CodeInvalidRequest:       "Invalid request.",
	CodeInvalidArgument:      "Invalid argument.",
	CodeUnauthorized:         "Access denied due to invalid subscription key.",
	CodeForbidden:            "Access forbidden due to policy or other configuration.",
	CodeNotFound:             "Resource not found.",
	CodeMethodNotAllowed:     "The requested HTTP method isn't allowed.",
	CodeConflict:             "The request couldn't be completed due to a conflict.",
	CodeUnsupportedMediaType: "Request content type isn't supported.",
	CodeInternalServerError:  "An unexpected error occurred.",
	CodeServiceUnavailable:   "A transient error occurred. Try again.",
}

// canonicalInnerMessages are the documented messages for the inner codes the gateway emits.
var canonicalInnerMessages = map[string]string{
	InnerInvalidContent:           "The file is corrupted or format is unsupported. Refer to documentation for the list of supported formats.",
	InnerInvalidContentLength:     "The input image is too large. Refer to documentation for the maximum file size.",
	InnerInvalidContentDimensions: "The input image dimensions are out of range. Refer to documentation for supported image dimensions.",
	InnerOperationNotFound:        "The requested operation wasn't found. The identifier is invalid or the operation is expired.",
	InnerModelNotFound:            "The requested model wasn't found. It was deleted or still building.",
	InnerContentSourceTimeout:     "Timeout while receiving the file from client.",
	InnerUnknown:                  "Unknown error.",
}

// CanonicalMessage returns Azure's documented message for a top-level DI code,
// or "" if the code has no documented message.
func CanonicalMessage(code string) string { return canonicalMessages[code] }

// statusForCode maps a top-level DI code to its documented HTTP status.
// Azure documents no row for 401 or 429; those are gateway-defined.
var statusForCode = map[string]int{
	CodeInvalidRequest:       http.StatusBadRequest,
	CodeInvalidArgument:      http.StatusBadRequest,
	CodeUnauthorized:         http.StatusUnauthorized,
	CodeForbidden:            http.StatusForbidden,
	CodeNotFound:             http.StatusNotFound,
	CodeMethodNotAllowed:     http.StatusMethodNotAllowed,
	CodeConflict:             http.StatusConflict,
	CodeUnsupportedMediaType: http.StatusUnsupportedMediaType,
	CodeInternalServerError:  http.StatusInternalServerError,
	CodeServiceUnavailable:   http.StatusServiceUnavailable,
}

// StatusForCode returns the documented HTTP status for a top-level DI code,
// defaulting to 500 for unknown codes.
func StatusForCode(code string) int {
	if s, ok := statusForCode[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// InnerError is DocumentIntelligenceInnerError. Unlike stock Azure.Core, DI's inner error has a
// message field. Nesting is unbounded in the schema; the gateway emits at most one level and
// caps parsing depth.
type InnerError struct {
	Code       string      `json:"code,omitempty"`
	Message    string      `json:"message,omitempty"`
	InnerError *InnerError `json:"innererror,omitempty"`
}

// Error is DocumentIntelligenceError. Only Code and Message are required.
//
// This same object appears in two places: as the body of a non-2xx DI response (wrapped in
// Response), and at HTTP 200 inside an operation envelope when status is "failed". Per-page
// failures go in Details with Target set to the page number as a string.
type Error struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Target     string      `json:"target,omitempty"`
	Details    []Error     `json:"details,omitempty"`
	InnerError *InnerError `json:"innererror,omitempty"`
}

// Response is DocumentIntelligenceErrorResponse — the wrapped DI error body.
type Response struct {
	Error Error `json:"error"`
}

// ReadError is ComputerVisionOcrError — the flat Read error body. No wrapper, no target,
// no details, no innererror.
type ReadError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

// APIError is a gateway error carrying everything needed to render it on either surface.
type APIError struct {
	Status     int
	Code       string // top-level code, in the vocabulary of the target surface
	Message    string
	Target     string
	InnerCode  string // DI only; ignored when rendering the Read shape
	InnerMsg   string // DI only
	Details    []Error
	RequestID  string // Read only
	RetryAfter int    // seconds; emitted only for retriable statuses. See WriteTo.
}

func (e *APIError) Error() string {
	return fmt.Sprintf("azure error %d %s: %s", e.Status, e.Code, e.Message)
}

// New builds an APIError, filling in Azure's canonical message when message is empty.
func New(status int, code, message string) *APIError {
	if message == "" {
		message = canonicalMessages[code]
	}
	return &APIError{Status: status, Code: code, Message: message}
}

// WithInner attaches a DI inner error, defaulting to the documented message for that inner code.
func (e *APIError) WithInner(code, message string) *APIError {
	if message == "" {
		message = canonicalInnerMessages[code]
	}
	e.InnerCode, e.InnerMsg = code, message
	return e
}

// WithRetryAfter marks the error retriable after n seconds.
//
// This is deliberately not honoured for non-retriable statuses in WriteTo: azure-core's RetryPolicy
// retries *any* response >= 400 that carries a Retry-After header, bypassing its method allowlist,
// up to 10 times. A 404 or 400 with Retry-After therefore turns one client call into eleven.
func (e *APIError) WithRetryAfter(seconds int) *APIError {
	e.RetryAfter = seconds
	return e
}

// retriable reports whether a Retry-After header is safe to emit alongside this status.
func retriable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Body renders the error in the wire shape of the given surface.
func (e *APIError) Body(s Surface) any {
	if s == SurfaceRead {
		return ReadError{Code: e.Code, Message: e.Message, RequestID: e.RequestID}
	}
	out := Response{Error: Error{
		Code:    e.Code,
		Message: e.Message,
		Target:  e.Target,
		Details: e.Details,
	}}
	if e.InnerCode != "" || e.InnerMsg != "" {
		out.Error.InnerError = &InnerError{Code: e.InnerCode, Message: e.InnerMsg}
	}
	return out
}

// WriteTo renders the error to w in the given surface's shape.
//
// Retry-After is emitted only when both requested and safe for the status; see WithRetryAfter.
func (e *APIError) WriteTo(w http.ResponseWriter, s Surface) {
	body, err := json.Marshal(e.Body(s))
	if err != nil { // unreachable for these types, but never emit a bodyless error
		body = []byte(`{"error":{"code":"InternalServerError","message":"An unexpected error occurred."}}`)
		if s == SurfaceRead {
			body = []byte(`{"code":"InternalServerError","message":"An unexpected error occurred."}`)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if e.RetryAfter > 0 && retriable(e.Status) {
		// Integer seconds only. An HTTP-date here hard-fails the Python DI client, which runs
		// the header through _deserialize("int", ...).
		h.Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	w.WriteHeader(e.Status)
	_, _ = w.Write(body)
}

// Constructors for the errors the gateway actually emits. Each picks the right vocabulary for
// its surface: DI uses the documented top-level/inner code pairs, Read uses the flat enum, which
// has no not-found member.

// NotFound is returned for an unknown or TTL-expired operation id.
//
// It is never returned for a live id: a 404 on an in-flight poll kills Java clients with an
// opaque NullPointerException and marks the operation terminally failed in .NET and JS.
func NotFound(s Surface) *APIError {
	if s == SurfaceRead {
		return &APIError{
			Status:  http.StatusNotFound,
			Code:    ReadInvalidRequest,
			Message: "Operation not found. The identifier is invalid or the operation is expired.",
		}
	}
	return New(http.StatusNotFound, CodeNotFound, "").
		WithInner(InnerOperationNotFound, "")
}

// ModelNotFound is returned for an unrecognised modelId on the DI surface.
func ModelNotFound(modelID string) *APIError {
	return New(http.StatusNotFound, CodeNotFound, "").
		WithInner(InnerModelNotFound, fmt.Sprintf("The requested model %q wasn't found.", modelID))
}

// BadRequest is a 400 with a surface-appropriate code.
func BadRequest(s Surface, message string) *APIError {
	if s == SurfaceRead {
		return &APIError{Status: http.StatusBadRequest, Code: ReadBadArgument, Message: message}
	}
	return New(http.StatusBadRequest, CodeInvalidRequest, message)
}

// InvalidParameter reports a bad query parameter value.
func InvalidParameter(s Surface, name, detail string) *APIError {
	if s == SurfaceRead {
		return &APIError{
			Status:  http.StatusBadRequest,
			Code:    ReadBadArgument,
			Message: fmt.Sprintf("The parameter %s is invalid: %s", name, detail),
		}
	}
	return New(http.StatusBadRequest, CodeInvalidArgument, "").
		WithInner(InnerInvalidParameter, fmt.Sprintf("The parameter %s is invalid: %s", name, detail))
}

// ContentTooLarge reports a body over the configured cap.
//
// It is emitted as 400, not 413: 413 is unmodelled on both surfaces, and when an nginx sidecar
// produces one it returns HTML rather than JSON.
func ContentTooLarge(s Surface) *APIError {
	if s == SurfaceRead {
		return &APIError{
			Status:  http.StatusBadRequest,
			Code:    ReadInvalidImageSize,
			Message: "The input image is too large.",
		}
	}
	return New(http.StatusBadRequest, CodeInvalidRequest, "").
		WithInner(InnerInvalidContentLength, "")
}

// UnsupportedMediaType reports a Content-Type the surface does not accept.
func UnsupportedMediaType(s Surface) *APIError {
	if s == SurfaceRead {
		return &APIError{
			Status:  http.StatusUnsupportedMediaType,
			Code:    ReadUnsupportedMediaType,
			Message: "Request content type isn't supported.",
		}
	}
	return New(http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "")
}

// MethodNotAllowed reports a verb the route does not serve.
func MethodNotAllowed(s Surface) *APIError {
	if s == SurfaceRead {
		return &APIError{
			Status:  http.StatusMethodNotAllowed,
			Code:    ReadInvalidRequest,
			Message: "The requested HTTP method isn't allowed.",
		}
	}
	return New(http.StatusMethodNotAllowed, CodeMethodNotAllowed, "")
}

// TooBusy is the admission-control rejection: every upstream slot is occupied.
//
// The upstream containers never emit 429 themselves — they do not cap TPS — so this is a
// gateway-defined response. Retry-After is mandatory here: the JS SDK retries a 429 only when
// one of the retry-after headers is present.
func TooBusy(s Surface, retryAfter int) *APIError {
	if s == SurfaceRead {
		return (&APIError{
			Status:  http.StatusTooManyRequests,
			Code:    ReadInvalidRequest,
			Message: "Too many requests in progress. Retry after the indicated interval.",
		}).WithRetryAfter(retryAfter)
	}
	return New(http.StatusTooManyRequests, CodeServiceUnavailable,
		"Too many requests in progress. Retry after the indicated interval.").
		WithRetryAfter(retryAfter)
}

// Unavailable reports a transient gateway-side failure, such as a full PVC.
func Unavailable(s Surface, message string, retryAfter int) *APIError {
	if s == SurfaceRead {
		return (&APIError{
			Status:  http.StatusServiceUnavailable,
			Code:    ReadUnspecified,
			Message: message,
		}).WithRetryAfter(retryAfter)
	}
	return New(http.StatusServiceUnavailable, CodeServiceUnavailable, message).
		WithRetryAfter(retryAfter)
}

// Internal reports an unexpected gateway-side failure.
func Internal(s Surface, message string) *APIError {
	if s == SurfaceRead {
		if message == "" {
			message = "An unexpected error occurred."
		}
		return &APIError{Status: http.StatusInternalServerError, Code: ReadInternalServerError, Message: message}
	}
	return New(http.StatusInternalServerError, CodeInternalServerError, message).
		WithInner(InnerUnknown, "")
}

// OperationError renders the error object embedded in a failed operation envelope at HTTP 200.
//
// The DI envelope carries the full error object. The Read envelope has no error member in its
// schema at all — a failed Read operation is just {"status":"failed", ...} — so callers must not
// serialise this into a Read envelope.
func (e *APIError) OperationError() Error {
	out := Error{Code: e.Code, Message: e.Message, Target: e.Target, Details: e.Details}
	if e.InnerCode != "" || e.InnerMsg != "" {
		out.InnerError = &InnerError{Code: e.InnerCode, Message: e.InnerMsg}
	}
	return out
}

// maxInnerDepth caps recursion when parsing an upstream innererror chain, which the schema
// leaves unbounded.
const maxInnerDepth = 3

// ParseUpstream extracts an error from an upstream container response body, tolerating every
// shape these containers are known to produce:
//
//   - the documented DI wrapped shape
//   - the flat DI shape the cloud service has been observed returning for an expired resultId,
//     which an MS moderator confirmed as a documentation/service divergence
//   - the flat Read shape
//   - {"status":"Failed"} with a capital F and no code or message, which is the only error form
//     documented for CV syncAnalyze
//
// An empty body, HTML from an intervening nginx, or anything else unparseable yields ok=false so
// the caller can synthesise a well-formed error rather than leaking the raw bytes to a client.
func ParseUpstream(body []byte, status int) (*APIError, bool) {
	if len(body) == 0 {
		return nil, false
	}

	var wrapped struct {
		Error *struct {
			Code       string  `json:"code"`
			Message    string  `json:"message"`
			Target     string  `json:"target"`
			Details    []Error `json:"details"`
			InnerError *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"innererror"`
		} `json:"error"`
		// Flat variants share the decode pass.
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, false
	}

	switch {
	case wrapped.Error != nil && wrapped.Error.Code != "":
		e := &APIError{
			Status:  status,
			Code:    wrapped.Error.Code,
			Message: wrapped.Error.Message,
			Target:  wrapped.Error.Target,
			Details: wrapped.Error.Details,
		}
		if wrapped.Error.InnerError != nil {
			e.InnerCode = wrapped.Error.InnerError.Code
			e.InnerMsg = wrapped.Error.InnerError.Message
		}
		return e, true

	case wrapped.Code != "":
		return &APIError{
			Status:    status,
			Code:      wrapped.Code,
			Message:   wrapped.Message,
			RequestID: wrapped.RequestID,
		}, true

	case wrapped.Status != "":
		// The syncAnalyze failure form: {"status":"Failed"}. Capital F, unactionable.
		// Compare case-insensitively — capital-F Failed and lowercase failed can both come from
		// the same container on different routes.
		if equalFold(wrapped.Status, "failed") {
			return &APIError{
				Status:  status,
				Code:    CodeInternalServerError,
				Message: "The upstream container reported the analysis as failed without detail.",
			}, true
		}
	}
	return nil, false
}

// equalFold compares two ASCII strings case-insensitively without allocating.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// AsAPIError extracts an *APIError from an error chain.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
