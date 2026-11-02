package cliproxy

import (
	"testing"
	"time"
)

func TestQuotaResetsShiftingByLessThanAMinuteAreTheSameReset(t *testing.T) {
	reset := time.Unix(1700000000, 0)

	if !quotaResetsClose(reset, reset.Add(59*time.Second)) {
		t.Fatal("drift from reset-after-seconds headers was treated as a new reset")
	}
	if quotaResetsClose(reset, reset.Add(time.Minute)) {
		t.Fatal("a reset a minute later was treated as the same reset")
	}
}
