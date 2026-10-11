package pdfsplit

import "bytes"

// headerWindow is how far into a body the PDF header may sit. Acrobat requires only that the
// header appear somewhere in the first 1024 bytes, so a file with a short prefix of junk (a mail
// or transfer wrapper) is still a PDF. Splitting spec §1 uses the same window for
// application/octet-stream bodies.
const headerWindow = 1024

var pdfMarker = []byte("%PDF-")

// IsPDF reports whether head (the first bytes of a body) contains "%PDF-" within 1024 bytes.
//
// It is only the cheap gate before parsing; the parse in Open is what proves a body is a PDF.
func IsPDF(head []byte) bool {
	if len(head) > headerWindow {
		head = head[:headerWindow]
	}
	return bytes.Contains(head, pdfMarker)
}
