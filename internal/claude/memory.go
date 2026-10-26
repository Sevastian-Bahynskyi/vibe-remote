package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type MemoryImport struct {
	Source       string `json:"source"`
	OriginalName string `json:"originalName"`
	Hash         string `json:"hash"`
	Target       string `json:"target"`
}

type SharedMemory struct {
	Directory       string         `json:"directory"`
	ProjectIdentity string         `json:"projectIdentity"`
	Imports         []MemoryImport `json:"imports"`
	Conflicts       int            `json:"conflicts"`
}

func canonicalProject(ctx context.Context, workspace string) (string, []string, error) {
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", nil, err
	}
	aliases := map[string]bool{workspace: true, canonical: true}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "git", "-C", canonical, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "path:" + canonical, []string{workspace, canonical}, nil
	}
	common, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		return "", nil, err
	}
	if filepath.Base(common) == ".git" {
		aliases[filepath.Dir(common)] = true
	}
	if root, err := exec.CommandContext(ctx, "git", "-C", canonical, "rev-parse", "--show-toplevel").Output(); err == nil {
		aliases[strings.TrimSpace(string(root))] = true
	}
	if data, err := exec.CommandContext(ctx, "git", "-C", canonical, "worktree", "list", "--porcelain").Output(); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if value, ok := strings.CutPrefix(line, "worktree "); ok {
				if strings.HasPrefix(value, "\"") {
					decoded, err := strconv.Unquote(value)
					if err != nil {
						continue
					}
					value = decoded
				}
				aliases[value] = true
			}
		}
	}
	paths := []string{}
	for path := range aliases {
		paths = append(paths, path)
		if strings.HasPrefix(path, "/private/var/") || strings.HasPrefix(path, "/private/tmp/") {
			alternative := strings.TrimPrefix(path, "/private")
			if actual, err := filepath.EvalSymlinks(alternative); err == nil {
				if expected, err := filepath.EvalSymlinks(path); err == nil && actual == expected {
					paths = append(paths, alternative)
				}
			}
		}
	}
	sort.Strings(paths)
	return "git:" + common, paths, nil
}

func claudeProjectKey(path string) string {
	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			return char
		}
		return '-'
	}, path)
}

func memorySourceDirectories(root, normal string, aliases []string) ([]string, error) {
	profiles := []string{}
	if normal != "" {
		profiles = append(profiles, normal)
	}
	entries, err := os.ReadDir(filepath.Join(root, "claude-profiles"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			profiles = append(profiles, filepath.Join(root, "claude-profiles", entry.Name()))
		}
	}
	keys := map[string]bool{}
	for _, alias := range aliases {
		keys[claudeProjectKey(alias)] = true
	}
	directories := map[string]bool{}
	for _, profile := range profiles {
		for key := range keys {
			path := filepath.Join(profile, "projects", key, "memory")
			info, err := os.Lstat(path)
			if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				directories[path] = true
			}
		}
	}
	result := []string{}
	for path := range directories {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func lockMemoryImport(ctx context.Context, path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "memory import lock")
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func PrepareProjectMemory(ctx context.Context, root, normal, workspace string) (SharedMemory, error) {
	identity, aliases, err := canonicalProject(ctx, workspace)
	if err != nil {
		return SharedMemory{}, err
	}
	directory := filepath.Join(root, "shared-memory", contentHash([]byte(identity)))
	if err := secureProfileParents(root, filepath.Join("shared-memory", contentHash([]byte(identity)), "imports.json")); err != nil {
		return SharedMemory{}, err
	}
	unlock, err := lockMemoryImport(ctx, filepath.Join(directory, ".import.lock"))
	if err != nil {
		return SharedMemory{}, err
	}
	defer unlock()
	manifest := SharedMemory{Directory: directory, ProjectIdentity: identity, Imports: []MemoryImport{}}
	if err := readJSONFile(filepath.Join(directory, "imports.json"), &manifest); err != nil {
		return manifest, errors.New("shared memory import index needs repair")
	}
	if manifest.ProjectIdentity != identity {
		return manifest, errors.New("shared memory project identity does not match")
	}
	manifest.Directory = directory
	known := map[string]bool{}
	targets := map[string]string{}
	for _, entry := range manifest.Imports {
		if filepath.IsAbs(entry.Target) || entry.Target == ".." || strings.HasPrefix(filepath.Clean(entry.Target), ".."+string(filepath.Separator)) {
			return manifest, ErrInvalidPath
		}
		known[entry.Source+"\x00"+entry.Hash] = true
		targets[entry.Hash] = entry.Target
	}
	err = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(path) != ".md" || filepath.Base(path) == "MEMORY.md" || filepath.Base(path) == "IMPORTS.md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(directory, path)
		if _, exists := targets[contentHash(data)]; !exists {
			targets[contentHash(data)] = relative
		}
		return nil
	})
	if err != nil {
		return manifest, err
	}
	sources, err := memorySourceDirectories(root, normal, aliases)
	if err != nil {
		return manifest, err
	}
	for _, source := range sources {
		err := filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".md" {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Size() > 16<<20 {
				return errors.New("a memory note is too large to import safely")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash := contentHash(data)
			if known[path+"\x00"+hash] {
				return nil
			}
			name, _ := filepath.Rel(source, path)
			target := targets[hash]
			if target == "" {
				filename := strings.Map(func(char rune) rune {
					if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
						return char
					}
					return '-'
				}, filepath.Base(name))
				target = filepath.Join("imports", "source-"+contentHash([]byte(source))[:10], hash[:12]+"-"+filename)
				if err := secureProfileParents(directory, target); err != nil {
					return err
				}
				if err := privateWrite(filepath.Join(directory, target), data, 0o600); err != nil {
					return err
				}
				targets[hash] = target
			}
			manifest.Imports = append(manifest.Imports, MemoryImport{Source: path, OriginalName: name, Hash: hash, Target: target})
			known[path+"\x00"+hash] = true
			return nil
		})
		if err != nil {
			return manifest, err
		}
	}
	names := map[string]map[string]bool{}
	for _, entry := range manifest.Imports {
		if names[entry.OriginalName] == nil {
			names[entry.OriginalName] = map[string]bool{}
		}
		names[entry.OriginalName][entry.Hash] = true
	}
	manifest.Conflicts = 0
	for _, versions := range names {
		if len(versions) > 1 {
			manifest.Conflicts++
		}
	}
	index := strings.Builder{}
	index.WriteString("# Imported profile memories\n\nOriginal files are unchanged. Identical content shares one imported copy. Differing versions are retained with provenance; none is chosen as authoritative.\n\n")
	written := map[string]bool{}
	for _, entry := range manifest.Imports {
		if written[entry.Target] {
			continue
		}
		written[entry.Target] = true
		label := strings.ReplaceAll(strings.ReplaceAll(entry.OriginalName, "[", "\\["), "]", "\\]")
		label = strings.ReplaceAll(label, "\n", " ")
		link := (&url.URL{Path: filepath.ToSlash(entry.Target)}).EscapedPath()
		conflict := ""
		if len(names[entry.OriginalName]) > 1 {
			conflict = " — DIFFERING VERSIONS: review together"
		}
		fmt.Fprintf(&index, "- [%s](%s)%s\n", label, link, conflict)
		for _, provenance := range manifest.Imports {
			if provenance.Target == entry.Target {
				encoded, _ := json.Marshal(provenance.Source)
				fmt.Fprintf(&index, "  - Source: %s\n", encoded)
			}
		}
	}
	if err := writePrivateJSON(filepath.Join(directory, "imports.json"), manifest); err != nil {
		return manifest, err
	}
	if err := privateWrite(filepath.Join(directory, "IMPORTS.md"), []byte(index.String()), 0o600); err != nil {
		return manifest, err
	}
	mainIndex := filepath.Join(directory, "MEMORY.md")
	current, err := os.ReadFile(mainIndex)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return manifest, err
	}
	if !strings.Contains(string(current), "](IMPORTS.md)") {
		current = append(current, []byte("\n- [Imported profile memories and differing versions](IMPORTS.md)\n")...)
		if err := privateWrite(mainIndex, current, 0o600); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

func applySessionMemory(settingsPath, directory string) error {
	settings := map[string]json.RawMessage{}
	if err := readJSONFile(settingsPath, &settings); err != nil {
		return err
	}
	value, _ := json.Marshal(directory)
	settings["autoMemoryDirectory"] = value
	return writePrivateJSON(settingsPath, settings)
}
