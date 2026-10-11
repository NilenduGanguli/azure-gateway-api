package mockazure

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// This file stands alone on purpose: it declares everything it uses, with a body prefix, so it
// neither depends on the failure-mode tests' helpers nor collides with the helpers of any test file
// added to this package later.

const (
	bodyDISync     = "/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	bodyDISubmit   = "/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30"
	bodyReadSync   = "/vision/v3.2/read/syncAnalyze"
	bodyReadSubmit = "/vision/v3.2/read/analyze"

	// bodyReadV32Prefix is the start of the Read v3.2 analyzeResult as the container writes it. The
	// order matters to byte-preserving consumers, and a Go map would have marshalled it
	// alphabetically.
	bodyReadV32Prefix = `{"version":"3.2.0","modelVersion":"2022-04-30","readResults":[` +
		`{"page":1,"angle":0,"width":800,"height":600,"unit":"pixel","lines":[{"boundingBox":[`
)

var bodyClient = &http.Client{Timeout: 30 * time.Second}

// bodyPost POSTs a document to path on the container and returns the status and the whole body.
func bodyPost(t *testing.T, c *Container, path string) (status int, header http.Header, raw []byte) {
	t.Helper()
	resp, err := bodyClient.Post(c.URL()+path, "application/pdf", strings.NewReader("%PDF-1.7 fake"))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response of %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header, raw
}

// bodySync returns the body of a synchronous call that must have answered 200.
func bodySync(t *testing.T, c *Container, path string) []byte {
	t.Helper()
	status, _, raw := bodyPost(t, c, path)
	if status != http.StatusOK {
		t.Fatalf("POST %s: status %d, want 200: %s", path, status, raw)
	}
	return raw
}

// bodyPolled submits to path, which is an asynchronous route or a synchronous one made to degrade
// to 202, follows its Operation-Location and returns the poll's body. The fakes under test are
// built with PollsBeforeSuccess 0, so the first poll is terminal.
func bodyPolled(t *testing.T, c *Container, path string) []byte {
	t.Helper()
	status, header, _ := bodyPost(t, c, path)
	loc := header.Get("Operation-Location")
	if status != http.StatusAccepted || loc == "" {
		t.Fatalf("POST %s: status %d, Operation-Location %q; want 202 and a location", path, status, loc)
	}
	resp, err := bodyClient.Get(loc)
	if err != nil {
		t.Fatalf("poll %s: %v", loc, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("poll %s: status %d, read error %v", loc, resp.StatusCode, err)
	}
	return raw
}

func bodyObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("body is not a JSON object (%v): %.300s", err, raw)
	}
	return m
}

func bodyObjectAt(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", what, v)
	}
	return m
}

func bodyArrayAt(t *testing.T, v any, what string, n int) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok || len(a) != n {
		t.Fatalf("%s is %v, want an array of %d", what, v, n)
	}
	return a
}

// bodyWantBox asserts v is a bounding box: eight coordinates.
func bodyWantBox(t *testing.T, v any, what string) {
	t.Helper()
	for i, n := range bodyArrayAt(t, v, what, 8) {
		if _, ok := n.(float64); !ok {
			t.Errorf("%s[%d] is %T, want a number", what, i, n)
		}
	}
}

// bodyWantReadV32 asserts result is a Computer Vision Read v3.2 analyzeResult, and none of Document
// Intelligence's Layout.
func bodyWantReadV32(t *testing.T, result map[string]any) {
	t.Helper()
	if result["version"] != "3.2.0" || result["modelVersion"] != "2022-04-30" {
		t.Errorf("version %v / modelVersion %v, want 3.2.0 / 2022-04-30",
			result["version"], result["modelVersion"])
	}
	layoutKeys := []string{"modelId", "apiVersion", "stringIndexType", "content", "pages", "figures"}
	for _, layoutKey := range layoutKeys {
		if _, ok := result[layoutKey]; ok {
			t.Errorf("Read result carries the Layout member %q; real Read v3.2 has none of them", layoutKey)
		}
	}

	page := bodyObjectAt(t, bodyArrayAt(t, result["readResults"], "readResults", 1)[0], "readResults[0]")
	if page["page"] != float64(1) || page["angle"] != float64(0) ||
		page["width"] != float64(800) || page["height"] != float64(600) || page["unit"] != "pixel" {
		t.Errorf("page header is %v, want page 1, angle 0, 800x600 pixel", page)
	}

	line := bodyObjectAt(t, bodyArrayAt(t, page["lines"], "lines", 1)[0], "lines[0]")
	if line["text"] != "mock line" {
		t.Errorf("line text is %v, want %q", line["text"], "mock line")
	}
	bodyWantBox(t, line["boundingBox"], "line boundingBox")

	words := bodyArrayAt(t, line["words"], "words", 2)
	for i, want := range []struct {
		text       string
		confidence float64
	}{{"mock", 0.99}, {"line", 0.98}} {
		word := bodyObjectAt(t, words[i], "word")
		if word["text"] != want.text || word["confidence"] != want.confidence {
			t.Errorf("word %d is %v, want %q at %v", i, word, want.text, want.confidence)
		}
		bodyWantBox(t, word["boundingBox"], "word boundingBox")
	}
}

// bodyReadResultOf asserts the operation envelope around a Read result and returns the result.
func bodyReadResultOf(t *testing.T, envelope map[string]any) map[string]any {
	t.Helper()
	if envelope["status"] != "succeeded" {
		t.Errorf("envelope status is %v, want succeeded", envelope["status"])
	}
	if _, ok := envelope["modelId"]; ok {
		t.Error("envelope carries a modelId; Read has none")
	}
	return bodyObjectAt(t, envelope["analyzeResult"], "analyzeResult")
}

func TestReadFakeReturnsARealV32Body(t *testing.T) {
	t.Run("synchronous route", func(t *testing.T) {
		c := NewRead(Options{})
		defer c.Close()
		envelope := bodyObject(t, bodySync(t, c, bodyReadSync))
		bodyWantReadV32(t, bodyReadResultOf(t, envelope))
	})

	t.Run("synchronous route, bare body", func(t *testing.T) {
		c := NewRead(Options{SyncBodyBare: true})
		defer c.Close()
		raw := bodySync(t, c, bodyReadSync)
		result := bodyObject(t, raw)
		bodyWantReadV32(t, result)
		if _, ok := result["status"]; ok {
			t.Error("a bare body must not carry the operation envelope's status")
		}
		if !strings.HasPrefix(string(raw), bodyReadV32Prefix) {
			t.Errorf("members are out of the container's order:\n%.300s", raw)
		}
	})

	t.Run("poll of an asynchronous operation", func(t *testing.T) {
		c := NewRead(Options{})
		defer c.Close()
		raw := bodyPolled(t, c, bodyReadSubmit)
		bodyWantReadV32(t, bodyReadResultOf(t, bodyObject(t, raw)))
		if !strings.Contains(string(raw), `"analyzeResult":`+bodyReadV32Prefix) {
			t.Errorf("analyzeResult members are out of the container's order:\n%.400s", raw)
		}
	})

	t.Run("poll of a degraded synchronous call", func(t *testing.T) {
		c := NewRead(Options{Sync: Sync202})
		defer c.Close()
		bodyWantReadV32(t, bodyReadResultOf(t, bodyObject(t, bodyPolled(t, c, bodyReadSync))))
	})
}

// The Read change must not leak into Document Intelligence, which keeps its Layout body on every
// route.
func TestDIFakeKeepsItsLayoutBody(t *testing.T) {
	wantLayout := func(t *testing.T, result map[string]any) {
		t.Helper()
		if result["modelId"] != "prebuilt-layout" || result["content"] != "mock content" {
			t.Errorf("analyzeResult is %v, want the Layout body", result)
		}
		if pages, ok := result["pages"].([]any); !ok || len(pages) == 0 {
			t.Errorf("Layout body has no pages: %v", result["pages"])
		}
		for _, readKey := range []string{"readResults", "version", "modelVersion"} {
			if _, ok := result[readKey]; ok {
				t.Errorf("Layout body carries the Read member %q", readKey)
			}
		}
	}

	t.Run("synchronous route", func(t *testing.T) {
		c := NewDI(Options{})
		defer c.Close()
		envelope := bodyObject(t, bodySync(t, c, bodyDISync))
		wantLayout(t, bodyObjectAt(t, envelope["analyzeResult"], "analyzeResult"))
	})
	t.Run("synchronous route, bare body", func(t *testing.T) {
		c := NewDI(Options{SyncBodyBare: true})
		defer c.Close()
		wantLayout(t, bodyObject(t, bodySync(t, c, bodyDISync)))
	})
	t.Run("poll", func(t *testing.T) {
		c := NewDI(Options{})
		defer c.Close()
		envelope := bodyObject(t, bodyPolled(t, c, bodyDISubmit))
		wantLayout(t, bodyObjectAt(t, envelope["analyzeResult"], "analyzeResult"))
	})
}

// PadResultBytes inflates a result so streaming paths see a large body. On Read the padding has to
// live in a member of the analyzeResult, or the body would stop being a single JSON document.
func TestPadResultBytesPadsBothBodiesAndKeepsThemValidJSON(t *testing.T) {
	const pad = 4096

	wantPadded := func(t *testing.T, raw []byte, result map[string]any) {
		t.Helper()
		if !json.Valid(raw) {
			t.Fatalf("padded body is not valid JSON: %.200s", raw)
		}
		padding, _ := result["padding"].(string)
		if len(padding) != pad || strings.Trim(padding, "x") != "" {
			t.Errorf("padding is %d bytes (want %d of 'x')", len(padding), pad)
		}
	}

	t.Run("read", func(t *testing.T) {
		t.Run("synchronous route", func(t *testing.T) {
			c := NewRead(Options{PadResultBytes: pad})
			defer c.Close()
			raw := bodySync(t, c, bodyReadSync)
			result := bodyReadResultOf(t, bodyObject(t, raw))
			wantPadded(t, raw, result)
			bodyWantReadV32(t, result)
		})
		t.Run("synchronous route, bare body", func(t *testing.T) {
			c := NewRead(Options{PadResultBytes: pad, SyncBodyBare: true})
			defer c.Close()
			raw := bodySync(t, c, bodyReadSync)
			result := bodyObject(t, raw)
			wantPadded(t, raw, result)
			bodyWantReadV32(t, result)
		})
		t.Run("poll", func(t *testing.T) {
			c := NewRead(Options{PadResultBytes: pad})
			defer c.Close()
			raw := bodyPolled(t, c, bodyReadSubmit)
			result := bodyReadResultOf(t, bodyObject(t, raw))
			wantPadded(t, raw, result)
			bodyWantReadV32(t, result)
		})
	})

	t.Run("document intelligence", func(t *testing.T) {
		t.Run("synchronous route", func(t *testing.T) {
			c := NewDI(Options{PadResultBytes: pad})
			defer c.Close()
			raw := bodySync(t, c, bodyDISync)
			wantPadded(t, raw, bodyObjectAt(t, bodyObject(t, raw)["analyzeResult"], "analyzeResult"))
		})
		t.Run("poll", func(t *testing.T) {
			c := NewDI(Options{PadResultBytes: pad})
			defer c.Close()
			raw := bodyPolled(t, c, bodyDISubmit)
			wantPadded(t, raw, bodyObjectAt(t, bodyObject(t, raw)["analyzeResult"], "analyzeResult"))
		})
	})
}
