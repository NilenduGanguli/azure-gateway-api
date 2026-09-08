package mockazure

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveMocks starts both fakes and holds them open so scripts/probe-containers.sh can be run
// against them without a cluster. It is skipped unless MOCKAZURE_LIVE is set.
//
//	MOCKAZURE_LIVE=1 go test ./internal/mockazure -run TestLiveMocks
//	set -a; . /tmp/mockurls.env; set +a; ./scripts/probe-containers.sh
//
// MOCK_DI_SYNC selects the Document Intelligence synchronous behaviour to simulate:
// 0 available, 1 degrades to 202, 2 absent (404), 3 absent (500), 4 model not found.
// MOCK_HOLD_SECONDS controls how long the fakes stay up (default 120).
func TestLiveMocks(t *testing.T) {
	if os.Getenv("MOCKAZURE_LIVE") == "" {
		t.Skip("set MOCKAZURE_LIVE=1 to hold the fake containers open")
	}
	di := NewDI(Options{Sync: SyncBehavior(atoi(os.Getenv("MOCK_DI_SYNC"))), PollsBeforeSuccess: 1})
	rd := NewRead(Options{Sync: Sync200})
	defer di.Close()
	defer rd.Close()

	fmt.Printf("DI_UPSTREAM_URL=%s\nREAD_UPSTREAM_URL=%s\n", di.URL(), rd.URL())
	_ = os.WriteFile("/tmp/mockurls.env",
		[]byte(fmt.Sprintf("DI_UPSTREAM_URL=%s\nREAD_UPSTREAM_URL=%s\n", di.URL(), rd.URL())), 0o600)

	hold := atoi(os.Getenv("MOCK_HOLD_SECONDS"))
	if hold <= 0 {
		hold = 120
	}
	t.Logf("holding the fake containers for %ds", hold)
	time.Sleep(time.Duration(hold) * time.Second)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
