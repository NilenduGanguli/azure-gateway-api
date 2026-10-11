package main

import (
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
)

// TestServeStillNeedsBothUpstreams pins a temporary guard. The configuration accepts one upstream,
// but the job manager still builds a runner for both surfaces, so a surface with no container
// would answer 202 for work that can never run. Plan task T10 deletes the guard and this test.
func TestServeStillNeedsBothUpstreams(t *testing.T) {
	di := config.Upstream{BaseURL: "http://layout:5000"}
	read := config.Upstream{BaseURL: "http://ocr:5000"}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{"both upstreams", config.Config{DI: di, Read: read}, false},
		{"document intelligence only", config.Config{DI: di}, true},
		{"read only", config.Config{Read: read}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireBothUpstreams(&tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("error is %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}
