package system

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KeepAwakeAssertionName labels the power assertion so any process — the CLI
// included — can tell whether this daemon is currently pinning the Mac awake.
const KeepAwakeAssertionName = "Vibe Remote keep-awake"

const defaultAwakeInterval = time.Minute

// AwakePolicy decides when the daemon keeps the Mac out of idle sleep.
//
// Remote Control only reaches a slot while that slot's process holds its
// connection open, and a sleeping Mac drops those connections. So availability
// requires wakefulness, and the lever left is not holding the assertion when
// there is nothing to stay available for.
type AwakePolicy string

const (
	// AwakeAuto holds the assertion only while slots are serving. Reachability
	// is unchanged: with no slots running there is nothing to reach.
	AwakeAuto AwakePolicy = "auto"
	// AwakeAlways holds it for the daemon's whole lifetime.
	AwakeAlways AwakePolicy = "always"
	// AwakeOff never holds it. Slots drop off Remote Control when the Mac sleeps.
	AwakeOff AwakePolicy = "off"
)

func ParseAwakePolicy(value string) AwakePolicy {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(AwakeAlways):
		return AwakeAlways
	case string(AwakeOff):
		return AwakeOff
	default:
		return AwakeAuto
	}
}

type AwakeOptions struct {
	Policy AwakePolicy
	// Demand reports whether slots are currently serving Remote Control.
	Demand func() bool
	// OnChange runs after the assertion is taken or dropped, so the caller can
	// record the transition.
	OnChange func(held bool)
	Interval time.Duration

	hold func(name string) (func() error, error)
	onAC func(context.Context) bool
}

type AwakeManager struct {
	options AwakeOptions
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once

	mu      sync.Mutex
	release func() error
	since   time.Time
	total   time.Duration
}

// StartAwakeManager keeps the Mac awake for exactly as long as the policy says
// it must, and never while running on battery: on battery, sleeping is what
// protects the charge.
func StartAwakeManager(parent context.Context, options AwakeOptions) *AwakeManager {
	if options.Policy == "" {
		options.Policy = AwakeAuto
	}
	if options.Interval <= 0 {
		options.Interval = defaultAwakeInterval
	}
	if options.Demand == nil {
		options.Demand = func() bool { return false }
	}
	if options.hold == nil {
		options.hold = holdSleepAssertion
	}
	if options.onAC == nil {
		options.onAC = onACPower
	}
	ctx, cancel := context.WithCancel(parent)
	manager := &AwakeManager{options: options, cancel: cancel, done: make(chan struct{})}
	go manager.loop(ctx)
	return manager
}

func (manager *AwakeManager) loop(ctx context.Context) {
	defer close(manager.done)
	ticker := time.NewTicker(manager.options.Interval)
	defer ticker.Stop()
	manager.apply(manager.wants(ctx))
	for {
		select {
		case <-ctx.Done():
			manager.apply(false)
			return
		case <-ticker.C:
			manager.apply(manager.wants(ctx))
		}
	}
}

func (manager *AwakeManager) wants(ctx context.Context) bool {
	if manager.options.Policy == AwakeOff {
		return false
	}
	if !manager.options.onAC(ctx) {
		return false
	}
	return manager.options.Policy == AwakeAlways || manager.options.Demand()
}

func (manager *AwakeManager) apply(held bool) {
	manager.mu.Lock()
	if held == (manager.release != nil) {
		manager.mu.Unlock()
		return
	}
	if held {
		release, err := manager.options.hold(KeepAwakeAssertionName)
		if err != nil {
			manager.mu.Unlock()
			return
		}
		manager.release = release
		manager.since = time.Now()
	} else {
		_ = manager.release()
		manager.release = nil
		manager.total += time.Since(manager.since)
	}
	manager.mu.Unlock()
	if manager.options.OnChange != nil {
		manager.options.OnChange(held)
	}
}

// Held reports whether the assertion is currently taken.
func (manager *AwakeManager) Held() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.release != nil
}

// HeldFor reports how long this daemon has kept the Mac awake in total.
func (manager *AwakeManager) HeldFor() time.Duration {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.release == nil {
		return manager.total
	}
	return manager.total + time.Since(manager.since)
}

func (manager *AwakeManager) Close() {
	manager.once.Do(func() {
		manager.cancel()
		<-manager.done
	})
}

// SleepAssertion is one live power assertion that keeps the Mac out of sleep,
// whoever owns it.
type SleepAssertion struct {
	PID     int    `json:"pid"`
	Process string `json:"process"`
	Kind    string `json:"kind"`
}

// sleepBlockingKinds are the assertion types that actually hold off sleep.
// Claude Code takes PreventUserIdleSystemSleep through its own `caffeinate -i`
// whenever a session is working, so a reading that only looked for this
// project's assertion would report an idle Mac that is in fact pinned awake.
var sleepBlockingKinds = map[string]bool{
	"PreventSystemSleep":         true,
	"PreventUserIdleSystemSleep": true,
	"NoIdleSleepAssertion":       true,
	"NetworkClientActive":        true,
}

var assertionLinePattern = regexp.MustCompile(`pid (\d+)\(([^)]+)\):\s*\[[^\]]*\]\s*[\d:]+\s*(\w+)`)

func assertionsText(ctx context.Context) string {
	output, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "assertions").Output()
	if err != nil {
		return ""
	}
	return string(output)
}

// KeepAwakeHeld reports whether a Vibe Remote assertion is live right now, read
// from the power manager so the CLI can observe a daemon in another process.
func KeepAwakeHeld(ctx context.Context) bool {
	return assertionHeld(ctx, KeepAwakeAssertionName)
}

func assertionHeld(ctx context.Context, name string) bool {
	return strings.Contains(assertionsText(ctx), name)
}

// SleepBlockers lists every process currently holding sleep off.
func SleepBlockers(ctx context.Context) []SleepAssertion {
	return parseSleepAssertions(assertionsText(ctx))
}

func parseSleepAssertions(text string) []SleepAssertion {
	seen := map[string]bool{}
	assertions := []SleepAssertion{}
	for _, match := range assertionLinePattern.FindAllStringSubmatch(text, -1) {
		kind := match[3]
		if !sleepBlockingKinds[kind] {
			continue
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		key := match[1] + kind
		if seen[key] {
			continue
		}
		seen[key] = true
		assertions = append(assertions, SleepAssertion{PID: pid, Process: match[2], Kind: kind})
	}
	return assertions
}
