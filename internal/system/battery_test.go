package system

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const batteryPlistFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<array>
	<dict>
		<key>CycleCount</key><integer>56</integer>
		<key>DesignCapacity</key><integer>6249</integer>
		<key>NominalChargeCapacity</key><integer>5871</integer>
		<key>AppleRawMaxCapacity</key><integer>5719</integer>
		<key>CurrentCapacity</key><integer>80</integer>
		<key>Temperature</key><integer>3035</integer>
		<key>IsCharging</key><false/>
		<key>ExternalConnected</key><true/>
		<key>MaximumDischargeCurrent</key><integer>18446744073709547276</integer>
		<key>BatteryState</key><data>AAAAAA==</data>
		<key>PowerTelemetryData</key>
		<dict>
			<key>SystemPowerIn</key><integer>5298</integer>
		</dict>
	</dict>
</array>
</plist>
`

func decodeFixture(t *testing.T) any {
	t.Helper()
	decoded, err := decodePlist(strings.NewReader(batteryPlistFixture))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	entries, ok := decoded.([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("expected a single battery entry, got %#v", decoded)
	}
	return entries[0]
}

func TestDecodePlistReadsBatteryFields(t *testing.T) {
	t.Parallel()
	battery := decodeFixture(t)

	cycles, ok := plistInt(battery, "CycleCount")
	if !ok || cycles != 56 {
		t.Fatalf("CycleCount = %d (ok=%v), want 56", cycles, ok)
	}
	if !plistBool(battery, "ExternalConnected") {
		t.Fatal("expected ExternalConnected to decode as true")
	}
	if plistBool(battery, "IsCharging") {
		t.Fatal("expected IsCharging to decode as false")
	}
	telemetry := plistDict(battery, "PowerTelemetryData")
	if power, ok := plistInt(telemetry, "SystemPowerIn"); !ok || power != 5298 {
		t.Fatalf("SystemPowerIn = %d (ok=%v), want 5298", power, ok)
	}
}

// IOKit reports some counters above math.MaxInt64. One unreadable field must
// not discard the rest of the gauge.
func TestDecodePlistKeepsGoingPastOversizedIntegers(t *testing.T) {
	t.Parallel()
	battery := decodeFixture(t)

	if _, ok := plistInt(battery, "MaximumDischargeCurrent"); ok {
		t.Fatal("expected the oversized counter to decode as unparsed text")
	}
	if capacity, ok := plistInt(battery, "NominalChargeCapacity"); !ok || capacity != 5871 {
		t.Fatalf("NominalChargeCapacity = %d (ok=%v), want 5871", capacity, ok)
	}
}

// system_profiler writes "96 %" with a non-breaking space, which Go's \s
// does not match. The fixture keeps the real bytes so the pattern cannot
// silently regress to reporting 0%.
func TestParseHealthPercent(t *testing.T) {
	t.Parallel()
	output := "      Health Information:\n          Cycle Count: 56\n          Condition: Normal\n          Maximum Capacity: 96\u00a0%\n"
	percent, ok := parseHealthPercent(output)
	if !ok || percent != 96 {
		t.Fatalf("parseHealthPercent = %d (ok=%v), want 96", percent, ok)
	}
	if _, ok := parseHealthPercent("no battery here"); ok {
		t.Fatal("expected missing capacity line to report not-found")
	}
}

func TestBatteryHistoryRoundTripsAndOrders(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "battery-history.jsonl")
	base := time.Now().UTC().Truncate(time.Second)
	newer := BatterySnapshot{Recorded: base, Label: "cli", HealthPercent: 96, CycleCount: 56, NominalCapacityMah: 5871}
	older := BatterySnapshot{Recorded: base.Add(-48 * time.Hour), Label: "serve-start", HealthPercent: 97, CycleCount: 54, NominalCapacityMah: 5893}

	if err := AppendBatterySnapshot(path, newer); err != nil {
		t.Fatalf("append newer: %v", err)
	}
	if err := AppendBatterySnapshot(path, older); err != nil {
		t.Fatalf("append older: %v", err)
	}

	history, err := LoadBatteryHistory(path)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(history))
	}
	if history[0].Label != "serve-start" {
		t.Fatalf("expected oldest sample first, got %q", history[0].Label)
	}
}

func TestLoadBatteryHistoryMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	history, err := LoadBatteryHistory(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("expected a missing history to read as empty, got %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("expected no samples, got %d", len(history))
	}
}

func TestSummarizeBatteryReportsWearAndDutyCycle(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	history := []BatterySnapshot{
		{Recorded: base.Add(-72 * time.Hour), HealthPercent: 98, CycleCount: 54, NominalCapacityMah: 5893,
			TemperatureCelsius: 30, SystemPowerMilliwatts: 5000, KeepAwakeHeld: true},
		{Recorded: base.Add(-24 * time.Hour), HealthPercent: 97, CycleCount: 55, NominalCapacityMah: 5880,
			TemperatureCelsius: 32, SystemPowerMilliwatts: 6000, KeepAwakeHeld: true},
		{Recorded: base, HealthPercent: 96, CycleCount: 56, NominalCapacityMah: 5871,
			TemperatureCelsius: 34, SystemPowerMilliwatts: 7000, KeepAwakeHeld: false},
	}

	report, ok := SummarizeBattery(history)
	if !ok {
		t.Fatal("expected a report from three samples")
	}
	if report.CyclesAdded != 2 {
		t.Fatalf("CyclesAdded = %d, want 2", report.CyclesAdded)
	}
	if report.HealthPercentDelta != -2 {
		t.Fatalf("HealthPercentDelta = %d, want -2", report.HealthPercentDelta)
	}
	if report.NominalMahDelta != -22 {
		t.Fatalf("NominalMahDelta = %d, want -22", report.NominalMahDelta)
	}
	if report.AverageTemperature != 32 {
		t.Fatalf("AverageTemperature = %.1f, want 32", report.AverageTemperature)
	}
	if report.AveragePowerWatts != 6 {
		t.Fatalf("AveragePowerWatts = %.1f, want 6", report.AveragePowerWatts)
	}
	if report.KeepAwakeDutyCycle < 0.66 || report.KeepAwakeDutyCycle > 0.67 {
		t.Fatalf("KeepAwakeDutyCycle = %.3f, want ~0.667", report.KeepAwakeDutyCycle)
	}
	if report.Span < 71*time.Hour {
		t.Fatalf("Span = %s, want ~72h", report.Span)
	}
}

// A sample the gauge could not answer for records zero. Folding it into the
// trend would report a capacity cliff that never happened.
func TestSummarizeBatteryIgnoresUnreadableSamples(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	history := []BatterySnapshot{
		{Recorded: base.Add(-48 * time.Hour), HealthPercent: 0, CycleCount: 0, NominalCapacityMah: 0},
		{Recorded: base.Add(-24 * time.Hour), HealthPercent: 97, CycleCount: 55, NominalCapacityMah: 5893},
		{Recorded: base, HealthPercent: 96, CycleCount: 56, NominalCapacityMah: 5871},
	}

	report, ok := SummarizeBattery(history)
	if !ok {
		t.Fatal("expected a report")
	}
	if report.HealthFirst != 97 || report.HealthLast != 96 || report.HealthPercentDelta != -1 {
		t.Fatalf("health trend = %d → %d (%+d), want 97 → 96 (-1)",
			report.HealthFirst, report.HealthLast, report.HealthPercentDelta)
	}
	if report.NominalMahDelta != -22 {
		t.Fatalf("NominalMahDelta = %d, want -22", report.NominalMahDelta)
	}
	if report.CyclesAdded != 1 {
		t.Fatalf("CyclesAdded = %d, want 1", report.CyclesAdded)
	}
}

func TestSummarizeBatteryEmptyHistory(t *testing.T) {
	t.Parallel()
	if _, ok := SummarizeBattery(nil); ok {
		t.Fatal("expected no report from an empty history")
	}
}
