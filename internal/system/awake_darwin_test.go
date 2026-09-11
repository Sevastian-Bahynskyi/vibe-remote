//go:build darwin && cgo

package system

import (
	"context"
	"testing"
)

// Exercises the real IOKit path: the assertion must reach the power manager,
// and pmset must name it, because that pairing is how the CLI observes a daemon
// running in another process. A probe-specific name keeps the test from seeing
// the assertion of a daemon that is genuinely running on this Mac.
func TestHoldSleepAssertionRegistersWithPowerManager(t *testing.T) {
	const probeName = KeepAwakeAssertionName + " (test probe)"
	ctx := context.Background()

	release, err := holdSleepAssertion(probeName)
	if err != nil {
		t.Fatalf("hold assertion: %v", err)
	}
	if !assertionHeld(ctx, probeName) {
		_ = release()
		t.Fatal("expected the power manager to report the held assertion")
	}
	if err := release(); err != nil {
		t.Fatalf("release assertion: %v", err)
	}
	if assertionHeld(ctx, probeName) {
		t.Fatal("expected the assertion to be gone after release")
	}
	if err := release(); err != nil {
		t.Fatalf("expected releasing twice to be safe, got %v", err)
	}
}
