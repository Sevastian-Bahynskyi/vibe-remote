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
		return errors.New("expected command: serve, install, account-add, account-login, hook, notify, hooks-install, or status")
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
	awake := systemstate.StartAwakeManager(ctx)
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
