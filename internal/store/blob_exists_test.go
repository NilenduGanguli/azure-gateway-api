package store

import (
	"testing"
)

// TestExistsDoesNotLeakDescriptors is a regression test.
//
// The crash-recovery pass checks whether a job's input document survived. It once did that with
// Open, which returns a live *os.File, and discarded the result — leaking one descriptor per
// orphaned job. A pod recovering a full batch after a restart would exhaust its descriptor limit
// exactly when it was least able to afford it.
func TestExistsDoesNotLeakDescriptors(t *testing.T) {
	s := newTestStore(t)
	id := "abcd1234-0000-4000-8000-000000000000"

	if s.Blob.Exists(id, KindInput) {
		t.Error("Exists reported a blob that was never written")
	}
	if err := s.Blob.WriteAll(id, KindInput, []byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Far more iterations than any descriptor limit would tolerate if this leaked.
	for i := 0; i < 5000; i++ {
		if !s.Blob.Exists(id, KindInput) {
			t.Fatalf("Exists returned false on iteration %d", i)
		}
	}

	if err := s.Blob.Remove(id, KindInput); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.Blob.Exists(id, KindInput) {
		t.Error("Exists reported a removed blob")
	}
}
