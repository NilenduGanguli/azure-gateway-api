package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Health is a snapshot of one upstream's state, surfaced on the admin endpoints.
type Health struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
	// Detail carries the container's own status message, or the reason it is unreachable.
	Detail string `json:"detail,omitempty"`
	// SyncAnalyze reports what the old clients learned about the synchronous route: "unknown"
	// before the first call, then "available" or "unavailable". Client leaves it empty, and so
	// omitted, because the synchronous route is the only one it uses; there is nothing to learn.
	SyncAnalyze string `json:"syncAnalyze,omitempty"`
	LatencyMS   int64  `json:"latencyMs,omitempty"`
}

// checkStatus fetches the container's /status endpoint, which validates its billing key without
// consuming a model query.
func (b *base) checkStatus(ctx context.Context) Health {
	h := Health{Name: b.name, URL: redactUserinfo(b.baseURL), SyncAnalyze: "unknown"}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	started := time.Now()
	req, err := b.newRequest(ctx, http.MethodGet, b.baseURL+"/status", nil)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	resp, err := b.client.Do(req)
	if err != nil {
		h.Detail = err.Error()
		return h
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	drain(resp)
	h.LatencyMS = time.Since(started).Milliseconds()
	h.OK = resp.StatusCode == http.StatusOK
	h.Detail = summarizeStatus(body, resp.StatusCode)
	return h
}

// summarizeStatus extracts the container's apiStatus fields when present.
func summarizeStatus(body []byte, code int) string {
	var s struct {
		APIStatus  string `json:"apiStatus"`
		APIMessage string `json:"apiStatusMessage"`
	}
	if err := jsonUnmarshal(body, &s); err == nil && s.APIStatus != "" {
		if s.APIMessage != "" {
			return s.APIStatus + ": " + s.APIMessage
		}
		return s.APIStatus
	}
	if code != http.StatusOK {
		return fmt.Sprintf("HTTP %d", code)
	}
	return ""
}

// Health reports whether the container is reachable, from its GET /status. That route validates
// the billing key without consuming a model query.
func (c *Client) Health(ctx context.Context) Health {
	h := c.b.checkStatus(ctx)
	h.SyncAnalyze = ""
	return h
}
