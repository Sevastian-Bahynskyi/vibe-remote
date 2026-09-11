package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BatterySnapshot is one observation of battery wear. Every field is readable
// by any process, so the daemon and the CLI record identical rows.
type BatterySnapshot struct {
	Recorded              time.Time `json:"recorded"`
	Label                 string    `json:"label"`
	HealthPercent         int64     `json:"healthPercent"`
	CycleCount            int64     `json:"cycleCount"`
	DesignCapacityMah     int64     `json:"designCapacityMah"`
	NominalCapacityMah    int64     `json:"nominalCapacityMah"`
	RawMaxCapacityMah     int64     `json:"rawMaxCapacityMah"`
	ChargePercent         int64     `json:"chargePercent"`
	TemperatureCelsius    float64   `json:"temperatureCelsius"`
	Charging              bool      `json:"charging"`
	OnACPower             bool      `json:"onAcPower"`
	SystemPowerMilliwatts int64     `json:"systemPowerMilliwatts"`
	KeepAwakeHeld         bool      `json:"keepAwakeHeld"`
	SleepBlocked          bool      `json:"sleepBlocked"`
	SleepBlockers         []string  `json:"sleepBlockers,omitempty"`
}

// ReadBattery samples the battery gauge. macOS smooths the percentage it shows
// in Settings, so the raw mAh capacities are recorded alongside it: those are
// the honest trend signal when the displayed percentage steps.
func ReadBattery(ctx context.Context, label string) (BatterySnapshot, error) {
	payload, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-a", "-r", "-n", "AppleSmartBattery").Output()
	if err != nil {
		return BatterySnapshot{}, fmt.Errorf("read battery gauge: %w", err)
	}
	decoded, err := decodePlist(bytes.NewReader(payload))
	if err != nil {
		return BatterySnapshot{}, fmt.Errorf("decode battery gauge: %w", err)
	}
	entries, ok := decoded.([]any)
	if !ok || len(entries) == 0 {
		return BatterySnapshot{}, fmt.Errorf("decode battery gauge: no AppleSmartBattery entry")
	}
	battery := entries[0]
	// One pmset read answers both questions: whether this project holds the Mac
	// awake, and whether anything else does.
	assertions := assertionsText(ctx)
	blockers := parseSleepAssertions(assertions)
	snapshot := BatterySnapshot{
		Recorded:      time.Now().UTC(),
		Label:         label,
		Charging:      plistBool(battery, "IsCharging"),
		OnACPower:     plistBool(battery, "ExternalConnected"),
		KeepAwakeHeld: strings.Contains(assertions, KeepAwakeAssertionName),
		SleepBlocked:  len(blockers) > 0,
		SleepBlockers: blockerNames(blockers),
	}
	snapshot.CycleCount, _ = plistInt(battery, "CycleCount")
	snapshot.DesignCapacityMah, _ = plistInt(battery, "DesignCapacity")
	snapshot.NominalCapacityMah, _ = plistInt(battery, "NominalChargeCapacity")
	snapshot.RawMaxCapacityMah, _ = plistInt(battery, "AppleRawMaxCapacity")
	snapshot.ChargePercent, _ = plistInt(battery, "CurrentCapacity")
	if centiCelsius, ok := plistInt(battery, "Temperature"); ok {
		snapshot.TemperatureCelsius = float64(centiCelsius) / 100
	}
	if telemetry := plistDict(battery, "PowerTelemetryData"); telemetry != nil {
		snapshot.SystemPowerMilliwatts, _ = plistInt(telemetry, "SystemPowerIn")
	}
	snapshot.HealthPercent = readHealthPercent(ctx)
	return snapshot, nil
}

// blockerNames collapses assertions to one entry per process, since a process
// holding two assertion types is still one thing keeping the Mac awake.
func blockerNames(assertions []SleepAssertion) []string {
	seen := map[string]bool{}
	names := []string{}
	for _, assertion := range assertions {
		if seen[assertion.Process] {
			continue
		}
		seen[assertion.Process] = true
		names = append(names, assertion.Process)
	}
	sort.Strings(names)
	return names
}

// system_profiler separates the number from the percent sign with U+00A0, which
// Go's \s does not match, so the sign is left out of the pattern entirely.
var healthPercentPattern = regexp.MustCompile(`Maximum Capacity:\s*(\d+)`)

// readHealthPercent returns the percentage macOS shows in Settings. It comes
// from system_profiler because no IOKit key holds the smoothed value.
func readHealthPercent(ctx context.Context) int64 {
	output, err := exec.CommandContext(ctx, "/usr/sbin/system_profiler", "SPPowerDataType").Output()
	if err != nil {
		return 0
	}
	percent, _ := parseHealthPercent(string(output))
	return percent
}

func parseHealthPercent(text string) (int64, bool) {
	match := healthPercentPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	percent, err := strconv.ParseInt(match[1], 10, 64)
	return percent, err == nil
}

// RecordBattery appends a snapshot to the history file. Recording is
// event-driven: the daemon writes on start, stop, and keep-awake transitions,
// and the battery command writes whenever it is run. Nothing polls the gauge.
func RecordBattery(ctx context.Context, historyPath, label string) (BatterySnapshot, error) {
	snapshot, err := ReadBattery(ctx, label)
	if err != nil {
		return BatterySnapshot{}, err
	}
	return snapshot, AppendBatterySnapshot(historyPath, snapshot)
}

func AppendBatterySnapshot(historyPath string, snapshot BatterySnapshot) error {
	line, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open battery history: %w", err)
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}

// LoadBatteryHistory reads recorded snapshots oldest first, skipping rows that
// predate a format change rather than failing the whole report.
func LoadBatteryHistory(historyPath string) ([]BatterySnapshot, error) {
	file, err := os.Open(historyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	snapshots := []BatterySnapshot{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var snapshot BatterySnapshot
		if json.Unmarshal([]byte(line), &snapshot) != nil {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(snapshots, func(left, right int) bool {
		return snapshots[left].Recorded.Before(snapshots[right].Recorded)
	})
	return snapshots, nil
}

// BatteryReport is the trend across recorded snapshots.
type BatteryReport struct {
	Samples            int
	Span               time.Duration
	First              BatterySnapshot
	Last               BatterySnapshot
	HealthFirst        int64
	HealthLast         int64
	HealthPercentDelta int64
	NominalFirst       int64
	NominalLast        int64
	NominalMahDelta    int64
	CyclesAdded        int64
	AverageTemperature float64
	AveragePowerWatts  float64
	KeepAwakeDutyCycle float64
	SleepBlockedCycle  float64
	SleepBlockers      []string
}

// bounds returns the first and last readable values of one field. A sample the
// gauge could not answer for reads as zero, and folding those into a trend
// would invent a cliff that never happened.
func bounds(snapshots []BatterySnapshot, value func(BatterySnapshot) int64) (int64, int64) {
	first, last := int64(0), int64(0)
	for _, snapshot := range snapshots {
		current := value(snapshot)
		if current <= 0 {
			continue
		}
		if first == 0 {
			first = current
		}
		last = current
	}
	return first, last
}

// SummarizeBattery compares the oldest and newest snapshots. The keep-awake
// duty cycle is the share of samples taken while the daemon held the Mac out of
// idle sleep, which is the lever this project actually controls.
func SummarizeBattery(snapshots []BatterySnapshot) (BatteryReport, bool) {
	if len(snapshots) == 0 {
		return BatteryReport{}, false
	}
	first, last := snapshots[0], snapshots[len(snapshots)-1]
	report := BatteryReport{
		Samples: len(snapshots),
		Span:    last.Recorded.Sub(first.Recorded),
		First:   first,
		Last:    last,
	}
	report.HealthFirst, report.HealthLast = bounds(snapshots, func(snapshot BatterySnapshot) int64 {
		return snapshot.HealthPercent
	})
	report.HealthPercentDelta = report.HealthLast - report.HealthFirst
	report.NominalFirst, report.NominalLast = bounds(snapshots, func(snapshot BatterySnapshot) int64 {
		return snapshot.NominalCapacityMah
	})
	report.NominalMahDelta = report.NominalLast - report.NominalFirst
	cycleFirst, cycleLast := bounds(snapshots, func(snapshot BatterySnapshot) int64 {
		return snapshot.CycleCount
	})
	report.CyclesAdded = cycleLast - cycleFirst
	temperatureTotal, temperatureCount := 0.0, 0
	powerTotal, powerCount := 0.0, 0
	awake, blocked := 0, 0
	blockers := map[string]bool{}
	for _, snapshot := range snapshots {
		if snapshot.SleepBlocked {
			blocked++
		}
		for _, name := range snapshot.SleepBlockers {
			blockers[name] = true
		}
		if snapshot.TemperatureCelsius > 0 {
			temperatureTotal += snapshot.TemperatureCelsius
			temperatureCount++
		}
		if snapshot.SystemPowerMilliwatts > 0 {
			powerTotal += float64(snapshot.SystemPowerMilliwatts) / 1000
			powerCount++
		}
		if snapshot.KeepAwakeHeld {
			awake++
		}
	}
	if temperatureCount > 0 {
		report.AverageTemperature = temperatureTotal / float64(temperatureCount)
	}
	if powerCount > 0 {
		report.AveragePowerWatts = powerTotal / float64(powerCount)
	}
	report.KeepAwakeDutyCycle = float64(awake) / float64(len(snapshots))
	report.SleepBlockedCycle = float64(blocked) / float64(len(snapshots))
	for name := range blockers {
		report.SleepBlockers = append(report.SleepBlockers, name)
	}
	sort.Strings(report.SleepBlockers)
	return report, true
}
