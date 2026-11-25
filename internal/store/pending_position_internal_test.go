package store

import "testing"

// TestPendingPositionForgottenOnRollback — если append откатился, выданный
// stream_ordering не должен остаться в pending навсегда: иначе safePosition
// прилипает к нему и /sync перестаёт отдавать новые события.
func TestPendingPositionForgottenOnRollback(t *testing.T) {
	t.Parallel()

	db := &DB{}
	db.registerPendingPosition(42)

	if len(db.pendingSnapshot()) != 1 {
		t.Fatalf("ожидали один pending, получили %v", db.pendingSnapshot())
	}

	db.forgetPendingPosition(42)

	if len(db.pendingSnapshot()) != 0 {
		t.Fatalf("pending не очищен: %v", db.pendingSnapshot())
	}
}
