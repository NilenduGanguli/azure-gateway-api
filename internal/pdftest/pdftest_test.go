package pdftest

import (
	"bytes"
	"context"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func TestMakeParsesWithDistinctWidths(t *testing.T) {
	data := Make(7)
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	ctx, err := api.ReadAndValidate(context.Background(), bytes.NewReader(data), conf)
	if err != nil {
		t.Fatalf("pdfcpu cannot read the generated PDF: %v", err)
	}
	if ctx.PageCount != 7 {
		t.Fatalf("PageCount = %d, want 7", ctx.PageCount)
	}
	dims, err := ctx.PageDims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dims) != 7 {
		t.Fatalf("got %d page dims, want 7", len(dims))
	}
	for i, d := range dims {
		if got := PageOf(d.Width); got != i+1 {
			t.Fatalf("page %d width %v maps to %d", i+1, d.Width, got)
		}
	}
}

func TestMakeIsDeterministic(t *testing.T) {
	if !bytes.Equal(Make(3), Make(3)) {
		t.Fatal("Make is not deterministic")
	}
}
