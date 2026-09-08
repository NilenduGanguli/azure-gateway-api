package upstream

import (
	"io"
	"strings"
)

type readCloser = io.ReadCloser

func nopCloser(s string) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
