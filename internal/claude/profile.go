package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ProfileReadiness struct {
	Ready       bool      `json:"ready"`
	Fingerprint string    `json:"fingerprint"`
	Conflicts   []string  `json:"conflicts,omitempty"`
	Missing     []string  `json:"missing,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type baselineFile struct {
	Hash   string `json:"hash"`
	Backup string `json:"backup,omitempty"`
}
type baselineManifest struct {
	Version           int                        `json:"version"`
	Fingerprint       string                     `json:"fingerprint"`
	Files             map[string]baselineFile    `json:"files"`
	Preferences       map[string]json.RawMessage `json:"preferences"`
	PreferenceBackups map[string]json.RawMessage `json:"preferenceBackups,omitempty"`
	Conflicts         []string                   `json:"conflicts,omitempty"`
	Plugins           map[string]bool            `json:"plugins"`
	Marketplaces      map[string]bool            `json:"marketplaces"`
}

type baselineContent struct {
	Data []byte
	Mode fs.FileMode
}

func contentHash(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func privateWrite(path string, data []byte, mode fs.FileMode) error {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			if info.Mode().Perm() != mode {
				return os.Chmod(path, mode)
			}
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".vibe-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return privateWrite(path, data, 0o600)
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func collectBaseline(source string) (map[string]baselineContent, error) {
	result := map[string]baselineContent{}
	var visit func(string, string, map[string]bool) error
	visit = func(path, relative string, parents map[string]bool) error {
		resolved, err := filepath.EvalSymlinks(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if parents[resolved] || len(parents) > 64 {
				return errors.New("shared instruction directory contains a cycle")
			}
			next := map[string]bool{}
			for key, value := range parents {
				next[key] = value
			}
			next[resolved] = true
			entries, err := os.ReadDir(resolved)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == ".env" || entry.Name() == ".credentials.json" {
					continue
				}
				if err := visit(filepath.Join(resolved, entry.Name()), filepath.Join(relative, entry.Name()), next); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > 16<<20 || len(result) >= 10000 {
			return errors.New("shared instruction file cannot be safely provisioned")
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if info.Mode()&0o111 != 0 {
			mode = 0o700
		}
		result[relative] = baselineContent{Data: data, Mode: mode}
		return nil
	}
	for _, name := range []string{"CLAUDE.md", "rules", "skills", "commands", "agents", "output-styles"} {
		if err := visit(filepath.Join(source, name), name, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func secureProfileParents(profile, relative string) error {
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(filepath.Clean(relative), ".."+string(filepath.Separator)) {
		return ErrInvalidPath
	}
	info, err := os.Lstat(profile)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidPath
	}
	current := profile
	parts := strings.Split(filepath.Dir(relative), string(filepath.Separator))
	for _, part := range parts {
		if part == "." {
			continue
		}
		if part == ".." || filepath.IsAbs(part) {
			return ErrInvalidPath
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("profile instruction directory needs repair before provisioning")
		}
	}
	return nil
}

func backupBaselineConflict(profile, relative string, data []byte) (string, error) {
	name := filepath.Join(".vibe-baseline", "backups", contentHash(data)[:16], relative)
	if err := secureProfileParents(profile, name); err != nil {
		return "", err
	}
	if err := privateWrite(filepath.Join(profile, name), data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

func reconcileBaseline(ctx context.Context, source, profile string, runner CommandRunner) (report ProfileReadiness, resultErr error) {
	report = ProfileReadiness{UpdatedAt: time.Now().UTC()}
	stateDir := filepath.Join(profile, ".vibe-baseline")
	if err := secureProfileParents(profile, filepath.Join(".vibe-baseline", "manifest.json")); err != nil {
		return report, err
	}
	defer func() {
		if resultErr != nil {
			report.Ready = false
			report.Missing = append(report.Missing, "Shared profile preparation did not complete. Repair setup before starting.")
		}
		_ = writePrivateJSON(filepath.Join(stateDir, "readiness.json"), report)
	}()
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return report, errors.New("shared Claude configuration is unavailable")
	}
	files, err := collectBaseline(source)
	if err != nil {
		return report, err
	}
	settings := map[string]json.RawMessage{}
	if err := readJSONFile(filepath.Join(source, "settings.json"), &settings); err != nil {
		return report, errors.New("shared Claude settings are malformed")
	}
	prefs := map[string]json.RawMessage{}
	for _, key := range []string{"theme", "outputStyle", "language", "spinnerTipsEnabled", "showTurnDuration", "showSpinnerTaskStatus", "terminalProgressBarEnabled"} {
		if value, ok := settings[key]; ok {
			prefs[key] = value
		}
	}
	plugins := map[string]bool{}
	if value := settings["enabledPlugins"]; value != nil {
		if err := json.Unmarshal(value, &plugins); err != nil {
			return report, errors.New("shared plugin selection is malformed")
		}
	}
	manifest := baselineManifest{Version: 1, Files: map[string]baselineFile{}, Preferences: map[string]json.RawMessage{}, Plugins: map[string]bool{}, Marketplaces: map[string]bool{}}
	if err := readJSONFile(filepath.Join(stateDir, "manifest.json"), &manifest); err != nil {
		return report, errors.New("profile baseline manifest needs repair")
	}
	for relative, file := range manifest.Files {
		if filepath.IsAbs(relative) || strings.HasPrefix(filepath.Clean(relative), "..") {
			return report, ErrInvalidPath
		}
		if file.Backup != "" && (filepath.IsAbs(file.Backup) || !strings.HasPrefix(filepath.Clean(file.Backup), filepath.Join(".vibe-baseline", "backups")+string(filepath.Separator))) {
			return report, ErrInvalidPath
		}
	}
	for key := range manifest.Preferences {
		switch key {
		case "theme", "outputStyle", "language", "spinnerTipsEnabled", "showTurnDuration", "showSpinnerTaskStatus", "terminalProgressBarEnabled":
		default:
			return report, ErrInvalidPath
		}
	}
	for id := range manifest.Plugins {
		if !pluginIDPattern.MatchString(id) {
			return report, ErrInvalidPath
		}
	}
	for name := range manifest.Marketplaces {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`).MatchString(name) {
			return report, ErrInvalidPath
		}
	}
	if manifest.Files == nil {
		manifest.Files = map[string]baselineFile{}
	}
	if manifest.Plugins == nil {
		manifest.Plugins = map[string]bool{}
	}
	if manifest.Marketplaces == nil {
		manifest.Marketplaces = map[string]bool{}
	}
	if manifest.PreferenceBackups == nil {
		manifest.PreferenceBackups = map[string]json.RawMessage{}
	}
	report.Conflicts = append(report.Conflicts, manifest.Conflicts...)
	if manifest.Preferences == nil {
		manifest.Preferences = map[string]json.RawMessage{}
	}
	if manifest.Version != 1 {
		return report, errors.New("profile baseline version is unsupported")
	}
	keys := make([]string, 0, len(files))
	fingerprints := map[string]string{}
	for path, content := range files {
		keys = append(keys, path)
		fingerprints[path] = contentHash(content.Data)
	}
	sort.Strings(keys)
	for _, relative := range keys {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := secureProfileParents(profile, relative); err != nil {
			return report, err
		}
		target := filepath.Join(profile, relative)
		content := files[relative]
		previous := manifest.Files[relative]
		current, err := os.ReadFile(target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		if err == nil && contentHash(current) != fingerprints[relative] && contentHash(current) != previous.Hash {
			backup, err := backupBaselineConflict(profile, relative, current)
			if err != nil {
				return report, err
			}
			if previous.Backup == "" {
				previous.Backup = backup
			}
			report.Conflicts = append(report.Conflicts, relative)
		}
		if err != nil || contentHash(current) != fingerprints[relative] {
			if err := privateWrite(target, content.Data, content.Mode); err != nil {
				return report, err
			}
		}
		if previous.Hash == "" && err == nil && contentHash(current) == fingerprints[relative] {
			continue
		}
		manifest.Files[relative] = baselineFile{Hash: fingerprints[relative], Backup: previous.Backup}
	}
	for relative, previous := range manifest.Files {
		root := strings.Split(filepath.ToSlash(relative), "/")[0]
		switch root {
		case "CLAUDE.md", "rules", "skills", "commands", "agents", "output-styles":
		default:
			return report, ErrInvalidPath
		}
		if _, ok := files[relative]; ok {
			continue
		}
		if err := secureProfileParents(profile, relative); err != nil {
			return report, err
		}
		path := filepath.Join(profile, relative)
		current, err := os.ReadFile(path)
		if err == nil && contentHash(current) == previous.Hash {
			if previous.Backup != "" {
				if err := secureProfileParents(profile, previous.Backup); err != nil {
					return report, err
				}
				original, err := os.ReadFile(filepath.Join(profile, previous.Backup))
				if err != nil {
					return report, err
				}
				if err := privateWrite(path, original, 0o600); err != nil {
					return report, err
				}
			} else if err := os.Remove(path); err != nil {
				return report, err
			}
		}
		delete(manifest.Files, relative)
	}
	profileSettings := map[string]json.RawMessage{}
	if err := readJSONFile(filepath.Join(profile, "settings.json"), &profileSettings); err != nil {
		return report, errors.New("profile settings need repair")
	}
	before, _ := json.Marshal(profileSettings)
	for key, previous := range manifest.Preferences {
		if _, ok := prefs[key]; !ok && string(profileSettings[key]) == string(previous) {
			if original, ok := manifest.PreferenceBackups[key]; ok {
				profileSettings[key] = original
				delete(manifest.PreferenceBackups, key)
			} else {
				delete(profileSettings, key)
			}
		}
	}
	ownedPreferences := map[string]json.RawMessage{}
	for key, value := range prefs {
		if current, ok := profileSettings[key]; ok && string(current) != string(value) && string(current) != string(manifest.Preferences[key]) {
			if _, err := backupBaselineConflict(profile, "settings.json", before); err != nil {
				return report, err
			}
			if _, exists := manifest.PreferenceBackups[key]; !exists {
				manifest.PreferenceBackups[key] = current
			}
			report.Conflicts = append(report.Conflicts, "settings.json:"+key)
		}
		if current, exists := profileSettings[key]; !exists || string(current) != string(value) || manifest.Preferences[key] != nil {
			ownedPreferences[key] = value
		}
		profileSettings[key] = value
	}
	manifest.Preferences = ownedPreferences
	unique := map[string]bool{}
	for _, entry := range report.Conflicts {
		unique[entry] = true
	}
	report.Conflicts = nil
	for entry := range unique {
		report.Conflicts = append(report.Conflicts, entry)
	}
	sort.Strings(report.Conflicts)
	manifest.Conflicts = report.Conflicts
	if err := writePrivateJSON(filepath.Join(profile, "settings.json"), profileSettings); err != nil {
		return report, err
	}
	if err := writePrivateJSON(filepath.Join(stateDir, "manifest.json"), manifest); err != nil {
		return report, err
	}
	marketplaces, err := reconcilePlugins(ctx, source, profile, plugins, &manifest, runner)
	if err != nil {
		return report, err
	}
	encoded, _ := json.Marshal(struct {
		Files        map[string]string
		Preferences  map[string]json.RawMessage
		Plugins      map[string]bool
		Marketplaces []marketplace
	}{fingerprints, prefs, plugins, marketplaces})
	report.Fingerprint = contentHash(encoded)
	manifest.Fingerprint = report.Fingerprint
	if err := provisionPermissions(filepath.Join(source, "settings.json"), filepath.Join(stateDir, "permissions-check.json")); err != nil {
		return report, err
	}
	if err := writePrivateJSON(filepath.Join(stateDir, "manifest.json"), manifest); err != nil {
		return report, err
	}
	report.Ready = true
	return report, nil
}

func readProfileReadiness(profile string) ProfileReadiness {
	var report ProfileReadiness
	if err := readJSONFile(filepath.Join(profile, ".vibe-baseline", "readiness.json"), &report); err != nil {
		report.Missing = []string{"Profile readiness needs repair."}
	}
	return report
}

func (m *Accounts) ReconcileProfile(ctx context.Context, accountID string) (ProfileReadiness, error) {
	account, err := m.account(ctx, accountID)
	if err != nil {
		return ProfileReadiness{}, err
	}
	if m.baselineSource == "" {
		return ProfileReadiness{Ready: true}, nil
	}
	if liveProfileRecord(filepath.Join(filepath.Dir(m.profilesRoot), "worker-pids"), accountID) {
		current, err := baselineCurrent(ctx, m.baselineSource, account.ProfileDir, m.runner)
		if err != nil || !current {
			return ProfileReadiness{Missing: []string{"Restart sessions for this account to apply the shared configuration."}}, errors.New("stop sessions for this account before repairing shared configuration")
		}
		return readProfileReadiness(account.ProfileDir), nil
	}
	report, err := reconcileBaseline(ctx, m.baselineSource, account.ProfileDir, m.runner)
	if err != nil {
		return report, fmt.Errorf("shared profile is not ready; repair setup before starting")
	}
	return report, nil
}

func liveProfileRecord(directory, accountID string) bool {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return true
		}
		var record workerRecord
		if json.Unmarshal(data, &record) != nil {
			record.PID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if record.AccountID != "" && record.AccountID != accountID {
			continue
		}
		if record.PID <= 1 {
			return true
		}
		process, err := os.FindProcess(record.PID)
		if err != nil {
			return true
		}
		err = process.Signal(syscall.Signal(0))
		if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, os.ErrProcessDone) {
			return true
		}
	}
	return false
}

func baselineCurrent(ctx context.Context, source, profile string, runner CommandRunner) (bool, error) {
	report := readProfileReadiness(profile)
	if !report.Ready {
		return false, nil
	}
	files, err := collectBaseline(source)
	if err != nil {
		return false, err
	}
	settings := map[string]json.RawMessage{}
	if err := readJSONFile(filepath.Join(source, "settings.json"), &settings); err != nil {
		return false, err
	}
	prefs := map[string]json.RawMessage{}
	for _, key := range []string{"theme", "outputStyle", "language", "spinnerTipsEnabled", "showTurnDuration", "showSpinnerTaskStatus", "terminalProgressBarEnabled"} {
		if value, ok := settings[key]; ok {
			prefs[key] = value
		}
	}
	plugins := map[string]bool{}
	if settings["enabledPlugins"] != nil {
		if err := json.Unmarshal(settings["enabledPlugins"], &plugins); err != nil {
			return false, err
		}
	}
	marketplaces, err := listMarketplaces(ctx, runner, source)
	if err != nil {
		return false, err
	}
	fingerprints := map[string]string{}
	for relative, content := range files {
		fingerprints[relative] = contentHash(content.Data)
		parent := filepath.Dir(filepath.Join(profile, relative))
		for parent != profile {
			info, err := os.Lstat(parent)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return false, nil
			}
			parent = filepath.Dir(parent)
		}
		data, err := os.ReadFile(filepath.Join(profile, relative))
		if err != nil || contentHash(data) != fingerprints[relative] {
			return false, nil
		}
	}
	encoded, _ := json.Marshal(struct {
		Files        map[string]string
		Preferences  map[string]json.RawMessage
		Plugins      map[string]bool
		Marketplaces []marketplace
	}{fingerprints, prefs, plugins, marketplaces})
	if contentHash(encoded) != report.Fingerprint {
		return false, nil
	}
	current := map[string]json.RawMessage{}
	if err := readJSONFile(filepath.Join(profile, "settings.json"), &current); err != nil {
		return false, err
	}
	for key, value := range prefs {
		if !bytes.Equal(current[key], value) {
			return false, nil
		}
	}
	data, err := pluginCommand(ctx, runner, profile, "plugin", "list", "--json")
	if err != nil {
		return false, err
	}
	var inventory []installedPlugin
	if err := json.Unmarshal(data, &inventory); err != nil {
		return false, err
	}
	ready := map[string]bool{}
	for _, entry := range inventory {
		if entry.Scope == "user" && entry.Enabled {
			ready[entry.ID] = true
		}
	}
	for id, enabled := range plugins {
		if enabled && !ready[id] {
			return false, nil
		}
	}
	return true, nil
}

func (m *Accounts) ProfileReadiness(ctx context.Context, accountID string) (ProfileReadiness, error) {
	account, err := m.account(ctx, accountID)
	if err != nil {
		return ProfileReadiness{}, err
	}
	if m.baselineSource == "" {
		return ProfileReadiness{Ready: true}, nil
	}
	return readProfileReadiness(account.ProfileDir), nil
}
