package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// Result is a successful analysis.
//
// The raw upstream response is left on disk at Path and Start/End delimit the analyzeResult
// member inside it, so the caller can compose its client-facing envelope with a range copy
// instead of loading a result that Azure permits to be 500 MB.
type Result struct {
	Path       string
	Start, End int64
	// Bare reports that the container answered a bare AnalyzeResult with no operation envelope
	// around it, so the range is the whole file. Which shape the Document Intelligence
	// container's undocumented synchronous route sends is unverified, and a caller that answers
	// a client the way the container would has to know which one it holds.
	Bare bool

	UpstreamMS     int64
	UpstreamStatus int
}

// Size is the length of the analyzeResult member.
func (r *Result) Size() int64 { return r.End - r.Start }

// Cleanup removes the temporary upstream response. It is safe to call more than once, and on nil.
func (r *Result) Cleanup() {
	if r == nil {
		return
	}
	if r.Path == "" {
		return
	}
	_ = os.Remove(r.Path)
	r.Path = ""
}

// jsonUnmarshal is a thin alias so the shared helpers read the same everywhere.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// ErrResultTooLarge reports an upstream response beyond the configured ceiling.
var ErrResultTooLarge = errors.New("upstream: response exceeds the configured maximum")

// defaultMaxResultBytes is the result ceiling when MAX_RESULT_BYTES is not set.
const defaultMaxResultBytes = 512 << 20

// resultLimit is the ceiling on one result: the configured one, or the default.
func (b *base) resultLimit() int64 {
	if b.maxBytes <= 0 {
		return defaultMaxResultBytes
	}
	return b.maxBytes
}

// downloadBody streams a response body to a scratch file, bounded by maxBytes.
//
// The body is never held in memory: Azure permits a 500 MB OCR JSON response, and buffering that
// per in-flight job would exhaust a pod running several workers.
func (b *base) downloadBody(resp *http.Response, prefix string) (path string, size int64, err error) {
	f, err := b.tempFile(tempPrefix(prefix))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	limit := b.resultLimit()
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("upstream: read response body: %w", err)
	}
	if n > limit {
		_ = os.Remove(f.Name())
		return "", 0, ErrResultTooLarge
	}
	if err := f.Sync(); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("upstream: sync response body: %w", err)
	}
	return f.Name(), n, nil
}

// operationView is what the gateway needs from an upstream operation envelope, without loading
// the envelope itself.
type operationView struct {
	Status string
	// Start and End delimit the analyzeResult member within the file, when present.
	Start, End int64
	HasResult  bool
	// Bare is set when the response is a bare AnalyzeResult rather than an operation envelope.
	Bare bool
	Err  *azerr.APIError
}

// inspectOperation reads an upstream response file and reports its operation status, the byte
// range of its analyzeResult, and any embedded error.
//
// The response may be either an operation envelope — {"status":…,"analyzeResult":{…}} — or, on
// the undocumented synchronous routes, a bare AnalyzeResult with no envelope at all. Both are
// handled: an object with no status member is treated as a bare result.
func inspectOperation(path string, size int64, surface azerr.Surface) (*operationView, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("upstream: reopen response: %w", err)
	}
	defer func() { _ = f.Close() }()

	view := &operationView{}

	statusStart, statusEnd, found, err := jsonx.ValueRange(f, size, "status")
	if err != nil {
		return nil, fmt.Errorf("upstream: scan response: %w", err)
	}
	if found {
		// Bounded like every other member read here. A status is a short enum string; an upstream
		// that sent a multi-megabyte one would otherwise be allocated in full.
		const maxStatusBytes = 1 << 10
		if statusEnd-statusStart > maxStatusBytes {
			return nil, fmt.Errorf("upstream: status member is %d bytes, which is not a status",
				statusEnd-statusStart)
		}
		raw := make([]byte, statusEnd-statusStart)
		if _, err := f.ReadAt(raw, statusStart); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("upstream: read status: %w", err)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			s = strings.Trim(string(raw), `" `)
		}
		view.Status = s
	}

	resStart, resEnd, hasResult, err := jsonx.ValueRange(f, size, "analyzeResult")
	if err != nil {
		return nil, fmt.Errorf("upstream: scan analyzeResult: %w", err)
	}
	if hasResult {
		view.Start, view.End, view.HasResult = resStart, resEnd, true
	} else if view.Status == "" {
		// No status and no analyzeResult member: the whole document is the result. This is the
		// bare-AnalyzeResult shape the undocumented synchronous routes may return.
		lead := make([]byte, min64(64, size))
		if _, err := f.ReadAt(lead, 0); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("upstream: read response lead: %w", err)
		}
		if jsonx.IsObject(lead) {
			view.Start, view.End, view.HasResult, view.Bare = 0, size, true, true
			view.Status = string(statusSucceeded)
		}
	}

	// A failed operation carries its detail in an error member on the DI surface. The Read
	// envelope has no error member in its schema at all, so a failed Read yields only the status.
	if isFailed(view.Status) {
		errStart, errEnd, hasErr, err := jsonx.ValueRange(f, size, "error")
		if err == nil && hasErr && errEnd-errStart <= maxErrorBody {
			raw := make([]byte, errEnd-errStart)
			if _, rerr := f.ReadAt(raw, errStart); rerr == nil || errors.Is(rerr, io.EOF) {
				var e azerr.Error
				if json.Unmarshal(raw, &e) == nil && e.Code != "" {
					view.Err = &azerr.APIError{
						Status:  http.StatusInternalServerError,
						Code:    e.Code,
						Message: e.Message,
						Target:  e.Target,
						Details: e.Details,
					}
					if e.InnerError != nil {
						view.Err.InnerCode = e.InnerError.Code
						view.Err.InnerMsg = e.InnerError.Message
					}
				}
			}
		}
		// Computer Vision Read puts its failure detail inside analyzeResult.errors rather than in a
		// top-level error object — its schema models no error member for a failed operation at all.
		// A probed container returns exactly that, so the detail is lifted out here instead of
		// being discarded in favour of a generic message.
		if view.Err == nil && view.HasResult {
			if e := firstResultError(f, view.Start, view.End-view.Start); e != nil {
				view.Err = e
			}
		}
		if view.Err == nil {
			view.Err = azerr.Internal(surface,
				"The upstream container reported the analysis as failed.")
		}
	}
	return view, nil
}

// Operation status strings as the containers emit them. Comparison is case-insensitive because
// the same container emits lowercase "failed" on its async routes and capital-F "Failed" from
// CV syncAnalyze.
type statusString string

const (
	statusSucceeded statusString = "succeeded"
	statusFailed    statusString = "failed"
)

func isFailed(s string) bool { return strings.EqualFold(s, string(statusFailed)) }

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// firstResultError lifts the first entry of analyzeResult.errors, which is where Computer Vision
// Read reports why an analysis failed.
func firstResultError(ra io.ReaderAt, start, size int64) *azerr.APIError {
	section := io.NewSectionReader(ra, start, size)
	errStart, errEnd, found, err := jsonx.ValueRange(section, size, "errors")
	if err != nil || !found {
		return nil
	}
	const maxErrorsBytes = 1 << 20
	if errEnd-errStart > maxErrorsBytes {
		return nil
	}
	raw := make([]byte, errEnd-errStart)
	if _, rerr := section.ReadAt(raw, errStart); rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil
	}
	var list []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 || list[0].Code == "" {
		return nil
	}
	return &azerr.APIError{
		Status:  http.StatusInternalServerError,
		Code:    list[0].Code,
		Message: list[0].Message,
	}
}
