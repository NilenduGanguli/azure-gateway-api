// Package pdftest builds small, valid PDFs for tests.
//
// Page i (1-based) has a MediaBox width of BaseWidth+i points. A split part carries the original
// page objects, so a fake container can recover which original pages a part holds from the
// widths alone, without trusting anything the gateway sends alongside the document.
package pdftest

import (
	"bytes"
	"fmt"
)

// BaseWidth is the width of a notional page 0.
const BaseWidth = 600

// Height is every page's height. Together with a width above BaseWidth it tells a page Make built
// from an ordinary one: no common paper size is 800 points tall.
const Height = 800

// PageOf maps a MediaBox width back to its original page number.
func PageOf(width float64) int { return int(width+0.5) - BaseWidth }

// Make returns a PDF with n pages. Page i's MediaBox is [0 0 BaseWidth+i Height] and its content
// stream draws the text "PAGE i".
func Make(n int) []byte {
	var b bytes.Buffer
	offsets := make([]int, 0, 3+2*n)
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")

	// Object 1 is the catalog, 2 the page tree, 3 the shared font, then one page object and one
	// content stream per page.
	var kids bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 1; i <= n; i++ {
		stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (PAGE %d) Tj ET", i)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] "+
			"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", BaseWidth+i, Height, 5+2*(i-1)))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, xref)
	return b.Bytes()
}
