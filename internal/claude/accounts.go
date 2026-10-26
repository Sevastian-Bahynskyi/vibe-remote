package claude

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AuthOptions struct {
	ClaudePath     string
	Runner         CommandRunner
	Now            func() time.Time
	BaselineSource string
}

type Accounts struct {
	store          AccountStore
	runner         CommandRunner
	profilesRoot   string
	now            func() time.Time
	baselineSource string
}

func NewAccounts(appDataRoot string, store AccountStore, options AuthOptions) (*Accounts, error) {
	if store == nil {
		return nil, errors.New("Claude accounts require a store")
	}
	root, err := safeRoot(appDataRoot)
	if err != nil {
		return nil, err
	}
	profiles := filepath.Join(root, "claude-profiles")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		return nil, fmt.Errorf("create Claude profile root: %w", err)
	}
	info, err := os.Lstat(profiles)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: Claude profile root must be a real directory", ErrInvalidPath)
	}
	if err := os.Chmod(profiles, 0o700); err != nil {
		return nil, fmt.Errorf("secure Claude profile root: %w", err)
	}
	runner := options.Runner
	if runner == nil {
		runner = &execRunner{override: strings.TrimSpace(options.ClaudePath)}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	source := options.BaselineSource
	if source == "-" {
		source = ""
		return &Accounts{store: store, runner: runner, profilesRoot: profiles, now: now}, nil
	}
	if source == "" && options.Runner == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		source = filepath.Join(home, ".claude")
	}
	return &Accounts{store: store, runner: runner, profilesRoot: profiles, now: now, baselineSource: source}, nil
}
