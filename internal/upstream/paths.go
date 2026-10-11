package upstream

import (
	"net/url"
	"strings"
)

// DIPathPrefix is the literal path segment the Document Intelligence surface is served under.
//
// It is load-bearing well beyond routing: azure-ai-documentintelligence (Python),
// Azure.AI.DocumentIntelligence (.NET), the Java SDK and the JS SDK all derive the operation id
// from Operation-Location with the hard-coded expression
//
//	[^:]+://[^/]+/documentintelligence/.+/([^?/]+)
//
// so a gateway that serves this surface under any other prefix breaks every one of them, even
// when polling itself would work.
const DIPathPrefix = "/documentintelligence"

// LegacyPathPrefix is the Form Recognizer family the same containers also serve.
const LegacyPathPrefix = "/formrecognizer"

// prefix returns the family to call, defaulting to the current one.
func prefix(p string) string {
	if p == LegacyPathPrefix {
		return LegacyPathPrefix
	}
	return DIPathPrefix
}

// ReadPathPrefix is the path the Computer Vision Read surface is served under.
const ReadPathPrefix = "/vision/v3.2/read"

// escapeSegment percent-encodes a single path segment.
//
// Encoding matters for more than correctness: an unencoded brace makes Python's pipeline raise
// inside str.format, and an unencoded brace, space, pipe or caret makes Java's new URI(path)
// throw, which silently demotes its poller instead of failing loudly.
func escapeSegment(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), ":", "%3A")
}
