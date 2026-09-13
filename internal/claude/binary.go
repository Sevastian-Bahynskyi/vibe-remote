package claude

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrNoClaudeBinary is returned when no Claude Code executable can be found.
// It is deliberately a value the caller can report rather than a reason to
// refuse to start: the dashboard has a row for exactly this, and a service that
// exits instead cannot show it. See FindBinary.
var ErrNoClaudeBinary = errors.New("Claude Code executable not found")

// FindBinary locates the Claude Code executable.
//
// It is called per operation rather than once at startup, and that is the point.
// Resolving once meant a service that started while Claude Code was installed
// kept working, and a service that started while it was missing exited during
// construction — so launchd respawned it into a loop and took the dashboard down
// with it, including the health row that would have named the cause. Resolving
// each time also means reinstalling the CLI heals a running service instead of
// needing a restart.
//
// The PATH copy wins. Claude Desktop's bundled copy is a fallback, because a
// broken or half-finished `npm install -g @anthropic-ai/claude-code` leaves no
// `claude` on PATH while the desktop app still ships a working one.
func FindBinary() (string, error) {
	if found, err := exec.LookPath("claude"); err == nil {
		return found, nil
	}
	for _, candidate := range fallbackBinaries() {
		if usableBinary(candidate) {
			return candidate, nil
		}
	}
	return "", ErrNoClaudeBinary
}

func fallbackBinaries() []string {
	candidates := make([]string, 0, 4)
	if home, err := os.UserHomeDir(); err == nil {
		// The official native installer's location.
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "claude"))
		candidates = append(candidates, bundledDesktopBinaries(home)...)
	}
	return candidates
}

// bundledDesktopBinaries lists the Claude Code copies Claude Desktop keeps,
// newest version first. The app stores each release in its own directory and
// keeps older ones, so the newest is the one to prefer.
func bundledDesktopBinaries(home string) []string {
	root := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			versions = append(versions, entry.Name())
		}
	}
	sort.Slice(versions, func(left, right int) bool {
		return compareVersions(versions[left], versions[right]) > 0
	})
	paths := make([]string, 0, len(versions))
	for _, version := range versions {
		paths = append(paths, filepath.Join(root, version, "claude.app", "Contents", "MacOS", "claude"))
	}
	return paths
}

// compareVersions orders dot-separated numeric versions, so 2.1.266 sorts above
// 2.1.9 rather than below it as a string comparison would.
func compareVersions(left, right string) int {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	for index := 0; index < len(leftParts) || index < len(rightParts); index++ {
		leftValue := versionPart(leftParts, index)
		rightValue := versionPart(rightParts, index)
		if leftValue != rightValue {
			if leftValue > rightValue {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionPart(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(parts[index])
	if err != nil {
		return -1
	}
	return value
}

func usableBinary(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return false
	}
	return true
}
