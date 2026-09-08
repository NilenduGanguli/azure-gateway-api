// Package ids mints and validates the operation identifiers the gateway hands to clients.
//
// The format is not cosmetic. The canonical Computer Vision Read sample does
// Guid.Parse(operationLocation.Substring(len-36)) in .NET and split("/")[-1] in Python, so a Read
// operation id must be exactly 36 characters and must parse as a GUID. Anything else — a short
// id, an opaque token, a trailing slash — breaks unmodified clients.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Len is the length of a canonical GUID string: 8-4-4-4-12 plus four hyphens.
const Len = 36

// New returns a canonical lowercase RFC 4122 version 4 GUID.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on the platforms this runs on; if it somehow does, there is no
		// safe way to mint an identifier and continuing would risk collisions.
		panic(fmt.Sprintf("ids: crypto/rand unavailable: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	var out [Len]byte
	enc := func(dst []byte, src []byte) { hex.Encode(dst, src) }
	enc(out[0:8], b[0:4])
	out[8] = '-'
	enc(out[9:13], b[4:6])
	out[13] = '-'
	enc(out[14:18], b[6:8])
	out[18] = '-'
	enc(out[19:23], b[8:10])
	out[23] = '-'
	enc(out[24:36], b[10:16])
	return string(out[:])
}

// Valid reports whether s is a canonical 36-character GUID in lowercase hyphenated form.
//
// Validation is strict on length and layout but accepts either hex case on input, so a client
// echoing back an uppercased id still resolves. The gateway only ever emits lowercase.
func Valid(s string) bool {
	if len(s) != Len {
		return false
	}
	for i := 0; i < Len; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isDigit := c >= '0' && c <= '9'
			isLower := c >= 'a' && c <= 'f'
			isUpper := c >= 'A' && c <= 'F'
			if !isDigit && !isLower && !isUpper {
				return false
			}
		}
	}
	return true
}

// Normalize lowercases a valid id so lookups are case-insensitive while storage stays canonical.
// It returns ok=false if s is not a well-formed GUID.
func Normalize(s string) (string, bool) {
	if !Valid(s) {
		return "", false
	}
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'F' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out), true
}
