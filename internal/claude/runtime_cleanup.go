package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

type runtimeHistory struct {
	Current  string `json:"current"`
	Previous string `json:"previous"`
}

var runtimeID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (m *Manager) rememberRuntime(process Process) {
	native, ok := process.(*execProcess)
	if !ok {
		return
	}
	directory := filepath.Dir(native.cmd.Path)
	if filepath.Dir(directory) != filepath.Join(filepath.Dir(m.profilesRoot), "claude-runtime") {
		return
	}
	id := filepath.Base(directory)
	if !runtimeID.MatchString(id) {
		return
	}
	path := filepath.Join(filepath.Dir(m.profilesRoot), "runtime-history.json")
	var history runtimeHistory
	if readJSONFile(path, &history) != nil {
		return
	}
	if history.Current == id {
		return
	}
	history.Previous, history.Current = history.Current, id
	_ = writePrivateJSON(path, history)
}

func (m *Manager) CleanupRuntimes(now time.Time) error {
	directory := filepath.Join(filepath.Dir(m.profilesRoot), "claude-runtime")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	protected := map[string]bool{}
	m.mu.RLock()
	for _, worker := range m.workers {
		if native, ok := worker.process.(*execProcess); ok && worker.live() {
			protected[filepath.Base(filepath.Dir(native.cmd.Path))] = true
		}
	}
	m.mu.RUnlock()
	var history runtimeHistory
	if err := readJSONFile(filepath.Join(filepath.Dir(m.profilesRoot), "runtime-history.json"), &history); err != nil {
		return err
	}
	protected[history.Current] = true
	protected[history.Previous] = true
	if runner, ok := m.runner.(*execRunner); ok {
		if path, err := runner.binaryPath(); err == nil {
			if file, err := os.Open(path); err == nil {
				hash := sha256.New()
				if _, err := io.Copy(hash, file); err == nil {
					protected[hex.EncodeToString(hash.Sum(nil))] = true
				}
				file.Close()
			}
		}
	}
	type candidate struct {
		name     string
		modified time.Time
	}
	candidates := []candidate{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !runtimeID.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{entry.Name(), info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modified.After(candidates[j].modified) })
	if history.Previous == "" {
		for _, candidate := range candidates {
			if !protected[candidate.name] {
				protected[candidate.name] = true
				break
			}
		}
	}
	for _, candidate := range candidates {
		if !protected[candidate.name] && candidate.modified.Before(now.Add(-7*24*time.Hour)) {
			if err := os.RemoveAll(filepath.Join(directory, candidate.name)); err != nil {
				return err
			}
		}
	}
	return nil
}
