package pdfsplit

import (
	"bytes"
	"testing"
)

func TestIsPDFLooksInTheFirst1024Bytes(t *testing.T) {
	junk := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }
	header := []byte("%PDF-1.7\n")

	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"header at 0", header, true},
		{"header after 100 junk bytes", append(junk(100), header...), true},
		{"marker ending on byte 1024", append(junk(1024-len("%PDF-")), "%PDF-"...), true},
		{"marker straddling byte 1024", append(junk(1020), header...), false},
		{"header after 1100 bytes", append(junk(1100), header...), false},
		{"marker without its dash", []byte("%PDF1.7"), false},
		{"empty", []byte{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPDF(tc.head); got != tc.want {
				t.Errorf("IsPDF = %v, want %v", got, tc.want)
			}
		})
	}
}
