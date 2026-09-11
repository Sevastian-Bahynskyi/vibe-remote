package system

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type assertionRecorder struct {
	mu       sync.Mutex
	holds    int
	releases int
	fail     bool
}

func (recorder *assertionRecorder) hold(string) (func() error, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.fail {
		return nil, errors.New("assertion denied")
	}
	recorder.holds++
	return func() error {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.releases++
		return nil
	}, nil
}

func (recorder *assertionRecorder) counts() (int, int) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.holds, recorder.releases
}

func newManager(t *testing.T, options AwakeOptions, recorder *assertionRecorder) *AwakeManager {
	t.Helper()
	options.hold = recorder.hold
	if options.onAC == nil {
		options.onAC = func(context.Context) bool { return true }
	}
	if options.Interval == 0 {
		options.Interval = time.Millisecond
	}
	manager := StartAwakeManager(context.Background(), options)
	t.Cleanup(manager.Close)
	return manager
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}

func TestAwakeAutoHoldsOnlyWhileSlotsServe(t *testing.T) {
	recorder := &assertionRecorder{}
	active := true
	var mu sync.Mutex
	manager := newManager(t, AwakeOptions{
		Policy: AwakeAuto,
		Demand: func() bool {
			mu.Lock()
			defer mu.Unlock()
			return active
		},
	}, recorder)

	waitFor(t, manager.Held)
	mu.Lock()
	active = false
	mu.Unlock()
	waitFor(t, func() bool { return !manager.Held() })

	holds, releases := recorder.counts()
	if holds != 1 || releases != 1 {
		t.Fatalf("expected one hold and one release, got holds=%d releases=%d", holds, releases)
	}
}

func TestAwakeAutoNeverHoldsOnBattery(t *testing.T) {
	recorder := &assertionRecorder{}
	manager := newManager(t, AwakeOptions{
		Policy: AwakeAuto,
		Demand: func() bool { return true },
		onAC:   func(context.Context) bool { return false },
	}, recorder)

	time.Sleep(20 * time.Millisecond)
	if manager.Held() {
		t.Fatal("expected no assertion while running on battery")
	}
	if holds, _ := recorder.counts(); holds != 0 {
		t.Fatalf("expected zero holds on battery, got %d", holds)
	}
}

func TestAwakeAlwaysHoldsWithoutDemand(t *testing.T) {
	recorder := &assertionRecorder{}
	manager := newManager(t, AwakeOptions{
		Policy: AwakeAlways,
		Demand: func() bool { return false },
	}, recorder)

	waitFor(t, manager.Held)
}

func TestAwakeOffNeverHolds(t *testing.T) {
	recorder := &assertionRecorder{}
	manager := newManager(t, AwakeOptions{
		Policy: AwakeOff,
		Demand: func() bool { return true },
	}, recorder)

	time.Sleep(20 * time.Millisecond)
	if manager.Held() {
		t.Fatal("expected no assertion under the off policy")
	}
}

func TestAwakeCloseReleasesAssertion(t *testing.T) {
	recorder := &assertionRecorder{}
	manager := newManager(t, AwakeOptions{
		Policy: AwakeAlways,
		Demand: func() bool { return true },
	}, recorder)

	waitFor(t, manager.Held)
	manager.Close()

	if _, releases := recorder.counts(); releases != 1 {
		t.Fatalf("expected the assertion released on shutdown, got %d releases", releases)
	}
	if manager.HeldFor() <= 0 {
		t.Fatal("expected a recorded keep-awake duration")
	}
}

func TestAwakeSurvivesAssertionFailure(t *testing.T) {
	recorder := &assertionRecorder{fail: true}
	manager := newManager(t, AwakeOptions{
		Policy: AwakeAlways,
		Demand: func() bool { return true },
	}, recorder)

	time.Sleep(20 * time.Millisecond)
	if manager.Held() {
		t.Fatal("expected no assertion recorded when IOKit refuses")
	}
}

// Real `pmset -g assertions` output. Claude Code's own `caffeinate -i` must be
// counted: a reading that saw only this project's assertion would call the Mac
// free to sleep while it was in fact pinned awake.
const assertionsFixture = `Assertion status system-wide:
   BackgroundTask                 0
   PreventSystemSleep             1
   PreventUserIdleSystemSleep     1
Listed by owning process:
   pid 37263(Claude): [0x000d34bd0001909d] 08:11:39 NoIdleSleepAssertion named: "Electron"
   pid 748(xcodebuild): [0x000da69f00019d7c] 00:05:45 PreventUserIdleSystemSleep named: "Xcode running tests."
   pid 25618(caffeinate): [0x000da7de00019d9b] 00:00:26 PreventUserIdleSystemSleep named: "caffeinate command-line tool"
   pid 831(mds_stores): [0x0000b00009d63000] 00:02:31 BackgroundTask named: "com.apple.metadata.mds_stores.power"
   pid 22836(vibe-remote): [0x000da75d00019d90] 02:35:00 NetworkClientActive named: "Vibe Remote keep-awake"
`

func TestParseSleepAssertionsFindsEveryBlocker(t *testing.T) {
	t.Parallel()
	assertions := parseSleepAssertions(assertionsFixture)

	if len(assertions) != 4 {
		t.Fatalf("expected 4 sleep-blocking assertions, got %d: %+v", len(assertions), assertions)
	}
	owners := map[string]string{}
	for _, assertion := range assertions {
		owners[assertion.Process] = assertion.Kind
	}
	if owners["caffeinate"] != "PreventUserIdleSystemSleep" {
		t.Fatalf("expected Claude Code's caffeinate to be counted, got %+v", owners)
	}
	if owners["vibe-remote"] != "NetworkClientActive" {
		t.Fatalf("expected this project's assertion to be counted, got %+v", owners)
	}
	if _, listed := owners["mds_stores"]; listed {
		t.Fatal("BackgroundTask does not block sleep and must not be counted")
	}
}

func TestParseSleepAssertionsWhenNothingBlocks(t *testing.T) {
	t.Parallel()
	if assertions := parseSleepAssertions("Assertion status system-wide:\n   PreventSystemSleep 0\n"); len(assertions) != 0 {
		t.Fatalf("expected no blockers, got %+v", assertions)
	}
}

func TestParseAwakePolicyDefaultsToAuto(t *testing.T) {
	t.Parallel()
	cases := map[string]AwakePolicy{
		"":         AwakeAuto,
		"auto":     AwakeAuto,
		"nonsense": AwakeAuto,
		" ALWAYS ": AwakeAlways,
		"off":      AwakeOff,
	}
	for value, want := range cases {
		if got := ParseAwakePolicy(value); got != want {
			t.Fatalf("ParseAwakePolicy(%q) = %q, want %q", value, got, want)
		}
	}
}
