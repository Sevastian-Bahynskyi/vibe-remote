package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/checkpoint"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/install"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/server"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vibe-remote:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected command: serve, install, account-add, account-login, hook, notify, hooks-install, battery, or status")
	}
	layout, err := paths.Resolve()
	if err != nil {
		return err
	}
	switch arguments[0] {
	case "serve":
		return serve(layout)
	case "install":
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return install.Install(layout, executable)
	case "account-add":
		return accountAdd(layout, arguments[1:])
	case "account-login":
		return accountLogin(layout, arguments[1:])
	case "hook":
		return runHook(layout, arguments[1:])
	case "notify":
		return runNotify(layout, arguments[1:])
	case "hooks-install":
		return install.InstallHooks(layout)
	case "battery":
		return battery(layout, arguments[1:])
	case "status":
		return status(layout)
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func serve(layout paths.Layout) error {
	if err := paths.Ensure(layout); err != nil {
		return err
	}
	repository, err := store.Open(layout.Database)
	if err != nil {
		return err
	}
	defer repository.Close()
	manager, err := claude.New(layout.Root, repository, claude.Options{})
	if err != nil {
		return err
	}
	defer manager.Close(context.Background())
	service, err := server.New(server.Options{Store: repository, Claude: manager, Layout: layout, Binary: layout.Binary})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	recordBattery(layout, "serve-start")
	defer recordBattery(layout, "serve-stop")
	go sampleBatteryDaily(ctx, layout)
	awake := systemstate.StartAwakeManager(ctx, systemstate.AwakeOptions{
		Policy:   systemstate.ParseAwakePolicy(os.Getenv("VIBE_REMOTE_AWAKE")),
		Demand:   func() bool { return manager.RunningCount() > 0 },
		OnChange: func(held bool) { recordBattery(layout, awakeLabel(held)) },
	})
	defer awake.Close()
	// Restoring slots waits on Claude Remote Control registration, which is
	// slow for a cold profile and serialized across slots, so it runs behind
	// the listener: the dashboard must stay reachable while workers come up.
	service.RestoreInBackground(ctx, func(err error) {
		fmt.Fprintln(os.Stderr, "vibe-remote: restore:", err)
	})
	result := make(chan error, 1)
	go func() { result <- service.ListenAndServe() }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return service.Shutdown(shutdownContext)
	}
}

func accountAdd(layout paths.Layout, arguments []string) error {
	flags := flag.NewFlagSet("account-add", flag.ContinueOnError)
	email := flags.String("email", "", "Claude account email")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*email) == "" {
		return errors.New("--email is required")
	}
	if err := paths.Ensure(layout); err != nil {
		return err
	}
	repository, err := store.Open(layout.Database)
	if err != nil {
		return err
	}
	defer repository.Close()
	manager, err := claude.New(layout.Root, repository, claude.Options{})
	if err != nil {
		return err
	}
	account, err := manager.Add(context.Background(), *email)
	if err != nil {
		return err
	}
	if err := install.InstallClaudeHooks(account.ProfileDir, layout.Binary); err != nil {
		return err
	}
	fmt.Printf("Authenticated Claude account %s.\n", account.Email)
	return nil
}

func accountLogin(layout paths.Layout, arguments []string) error {
	flags := flag.NewFlagSet("account-login", flag.ContinueOnError)
	id := flags.String("id", "", "Claude account ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return errors.New("--id is required")
	}
	repository, err := store.Open(layout.Database)
	if err != nil {
		return err
	}
	defer repository.Close()
	manager, err := claude.New(layout.Root, repository, claude.Options{})
	if err != nil {
		return err
	}
	account, err := manager.Login(context.Background(), *id)
	if err != nil {
		return err
	}
	if err := install.InstallClaudeHooks(account.ProfileDir, layout.Binary); err != nil {
		return err
	}
	fmt.Printf("Authenticated Claude account %s.\n", account.Email)
	return nil
}

func runHook(layout paths.Layout, arguments []string) error {
	if len(arguments) != 2 {
		return errors.New("usage: vibe-remote hook <claude|codex> <event>")
	}
	provider, err := parseProvider(arguments[0])
	if err != nil {
		return err
	}
	repository, err := store.OpenWithOptions(layout.Database, store.Options{BusyTimeout: 500 * time.Millisecond})
	if err != nil {
		return err
	}
	defer repository.Close()
	service := checkpoint.NewService(repository, nil)
	hookContext, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	response, err := service.HandleStdin(hookContext, provider, os.Getenv("VIBE_REMOTE_ACCOUNT_ID"), os.Stdin)
	if err != nil {
		return err
	}
	if provider == model.ProviderCodex {
		if fingerprint, fingerprintErr := systemstate.HookFingerprint(layout.CodexHooks, layout.Binary); fingerprintErr == nil {
			_ = os.WriteFile(layout.CodexHookVerified, []byte(fingerprint+"\n"), 0o600)
		}
	}
	if len(response.Output) > 0 {
		_, err = os.Stdout.Write(append(response.Output, '\n'))
		if err != nil {
			_ = response.Release(hookContext)
			return err
		}
		return response.Finalize(hookContext)
	}
	return nil
}

func runNotify(layout paths.Layout, arguments []string) error {
	if len(arguments) < 2 || arguments[0] != "codex" {
		return errors.New("usage: vibe-remote notify codex [forwarded-config] <json>")
	}
	payload := arguments[len(arguments)-1]
	forwarded := ""
	if len(arguments) > 2 {
		forwarded = arguments[1]
	}
	repository, err := store.Open(layout.Database)
	if err == nil {
		service := checkpoint.NewService(repository, nil)
		_, err = service.HandleCodexNotify(context.Background(), "", payload)
		_ = repository.Close()
	}
	forwardErr := forwardNotification(forwarded, payload)
	if err != nil {
		return err
	}
	return forwardErr
}

func forwardNotification(encoded, payload string) error {
	command, err := install.DecodeForwardedNotify(encoded)
	if err != nil || len(command) == 0 {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], append(command[1:], payload)...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func awakeLabel(held bool) string {
	if held {
		return "keep-awake-hold"
	}
	return "keep-awake-release"
}

// sampleBatteryDaily keeps the wear history dense enough to show a trend. Under
// the auto policy a busy Mac never changes keep-awake state, so event-driven
// samples alone would leave a month of uptime holding a single row.
func sampleBatteryDaily(ctx context.Context, layout paths.Layout) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recordBattery(layout, "daily")
		}
	}
}

// recordBattery appends one gauge reading to the history. Failures are ignored:
// wear tracking must never take the daemon down.
func recordBattery(layout paths.Layout, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = systemstate.RecordBattery(ctx, layout.BatteryHistory, label)
}

func battery(layout paths.Layout, arguments []string) error {
	flags := flag.NewFlagSet("battery", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print every recorded sample as JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if err := paths.Ensure(layout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	current, err := systemstate.RecordBattery(ctx, layout.BatteryHistory, "cli")
	if err != nil {
		return err
	}
	history, err := systemstate.LoadBatteryHistory(layout.BatteryHistory)
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(history)
	}
	printBattery(current, history)
	return nil
}

func printBattery(current systemstate.BatterySnapshot, history []systemstate.BatterySnapshot) {
	fmt.Println("Battery")
	fmt.Printf("  Health (macOS)     %d%%\n", current.HealthPercent)
	fmt.Printf("  Gauge capacity     %d of %d mAh design (%s raw)\n",
		current.NominalCapacityMah, current.DesignCapacityMah,
		percent(current.NominalCapacityMah, current.DesignCapacityMah))
	fmt.Printf("  Cycles             %d\n", current.CycleCount)
	fmt.Printf("  Charge             %d%% (%s)\n", current.ChargePercent, chargeState(current))
	fmt.Printf("  Temperature        %.1f °C\n", current.TemperatureCelsius)
	fmt.Printf("  System draw        %.1f W\n", float64(current.SystemPowerMilliwatts)/1000)
	fmt.Printf("  Keep-awake (ours)  %s\n", awakeState(current.KeepAwakeHeld))
	fmt.Printf("  Sleep blocked      %s\n", blockedState(current))

	report, ok := systemstate.SummarizeBattery(history)
	if !ok || report.Samples < 2 {
		fmt.Println("\nTrend")
		fmt.Println("  Not enough samples yet. Run this again after a few days.")
		return
	}
	fmt.Printf("\nTrend (%d samples over %s)\n", report.Samples, duration(report.Span))
	fmt.Printf("  Health             %d%% → %d%% (%+d)\n",
		report.HealthFirst, report.HealthLast, report.HealthPercentDelta)
	fmt.Printf("  Gauge capacity     %d → %d mAh (%+d)\n",
		report.NominalFirst, report.NominalLast, report.NominalMahDelta)
	fmt.Printf("  Cycles added       %+d\n", report.CyclesAdded)
	fmt.Printf("  Avg temperature    %.1f °C\n", report.AverageTemperature)
	fmt.Printf("  Avg system draw    %.1f W\n", report.AveragePowerWatts)
	fmt.Printf("  Keep-awake duty    %.0f%% of samples (this daemon)\n", report.KeepAwakeDutyCycle*100)
	fmt.Printf("  Sleep blocked      %.0f%% of samples (anything)\n", report.SleepBlockedCycle*100)
	if len(report.SleepBlockers) > 0 {
		fmt.Printf("  Blocked by         %s\n", strings.Join(report.SleepBlockers, ", "))
	}
}

func blockedState(snapshot systemstate.BatterySnapshot) string {
	if !snapshot.SleepBlocked {
		return "no — the Mac can idle-sleep"
	}
	if len(snapshot.SleepBlockers) == 0 {
		return "yes"
	}
	return "yes — " + strings.Join(snapshot.SleepBlockers, ", ")
}

func percent(value, total int64) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(value)/float64(total)*100)
}

func chargeState(snapshot systemstate.BatterySnapshot) string {
	switch {
	case snapshot.Charging:
		return "charging"
	case snapshot.OnACPower:
		return "on AC, holding"
	default:
		return "on battery"
	}
}

func awakeState(held bool) string {
	if held {
		return "held — idle sleep is blocked right now"
	}
	return "released — the Mac may sleep"
}

func duration(span time.Duration) string {
	days := int(span.Hours()) / 24
	hours := int(span.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, int(span.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(span.Minutes()))
}

func status(layout paths.Layout) error {
	repository, err := store.Open(layout.Database)
	if err != nil {
		return err
	}
	defer repository.Close()
	accounts, err := repository.ListAccounts(context.Background())
	if err != nil {
		return err
	}
	workspaces, err := repository.ListWorkspaces(context.Background())
	if err != nil {
		return err
	}
	sessions, err := repository.ListSessions(context.Background(), store.SessionFilter{Limit: 20})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"version": version, "accounts": accounts, "workspaces": workspaces,
		"sessions": sessions, "system": systemstate.Inspect(context.Background(), layout.CodexHooks, layout.CodexHookVerified, layout.Binary),
	})
}

func parseProvider(value string) (model.Provider, error) {
	provider := model.Provider(strings.ToLower(strings.TrimSpace(value)))
	if provider != model.ProviderClaude && provider != model.ProviderCodex {
		return "", errors.New("provider must be claude or codex")
	}
	return provider, nil
}
