package ids

import (
	"strings"
	"testing"
)

// TestNewIsCanonicalGUID matters because the .NET Computer Vision sample does
// Substring(len-36) followed by Guid.Parse on the Operation-Location.
func TestNewIsCanonicalGUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id := New()
		if len(id) != Len {
			t.Fatalf("id %q is %d chars, want %d", id, len(id), Len)
		}
		if !Valid(id) {
			t.Fatalf("generated id %q fails its own validator", id)
		}
		if strings.ToLower(id) != id {
			t.Fatalf("id %q is not lowercase", id)
		}
		if id[14] != '4' {
			t.Fatalf("id %q is not version 4", id)
		}
		if c := id[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
			t.Fatalf("id %q has the wrong RFC 4122 variant nibble %q", id, c)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q after %d draws", id, i)
		}
		seen[id] = true
	}
}

func TestValidRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "short",
		"00000000-0000-4000-8000-00000000000",   // 35 chars
		"00000000-0000-4000-8000-0000000000000", // 37 chars
		"00000000_0000-4000-8000-000000000000",  // wrong separator
		"0000000g-0000-4000-8000-000000000000",  // non-hex
		"00000000-0000-4000-8000-000000000000/", // trailing slash
	} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
	}
}

func TestNormalizeLowercases(t *testing.T) {
	got, ok := Normalize("ABCDEF01-2345-4678-89AB-CDEF01234567")
	if !ok {
		t.Fatal("Normalize rejected a valid uppercase GUID")
	}
	if got != "abcdef01-2345-4678-89ab-cdef01234567" {
		t.Errorf("Normalize returned %q", got)
	}
	if _, ok := Normalize("not-a-guid"); ok {
		t.Error("Normalize accepted a malformed id")
	}
}
