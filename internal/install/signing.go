package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/paths"
)

var signingFingerprint = regexp.MustCompile(`^[A-Fa-f0-9]{40}$`)
var signingIdentityLine = regexp.MustCompile(`(?m)^\s*\d+\) ([A-F0-9]{40}) "([^"]+)"`)

func selectSigningIdentity(output string) string {
	identities := signingIdentityLine.FindAllStringSubmatch(output, -1)
	for _, prefix := range []string{"Developer ID Application:", "Apple Development:"} {
		for _, identity := range identities {
			if strings.HasPrefix(identity[2], prefix) {
				return identity[1]
			}
		}
	}
	return ""
}

func signInstalledExecutable(layout paths.Layout, executable string) error {
	identityPath := filepath.Join(layout.Root, "signing-identity")
	identity := strings.TrimSpace(os.Getenv("VIBE_REMOTE_SIGNING_IDENTITY"))
	if identity == "" {
		data, err := os.ReadFile(identityPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("could not read the saved signing identity")
		}
		identity = strings.TrimSpace(string(data))
	}
	if identity == "" {
		output, err := exec.Command("security", "find-identity", "-v", "-p", "codesigning").Output()
		if err != nil {
			return errors.New("could not inspect available signing identities")
		}
		identity = selectSigningIdentity(string(output))
	}
	if !signingFingerprint.MatchString(identity) {
		return errors.New("stable signing requires an Apple Development or Developer ID certificate; set VIBE_REMOTE_SIGNING_IDENTITY to its 40-character fingerprint")
	}
	if err := exec.Command("codesign", "--force", "--sign", identity, "--identifier", paths.ServiceLabel, "--timestamp=none", executable).Run(); err != nil {
		return fmt.Errorf("sign Vibe Remote with its saved identity: %w", err)
	}
	if err := exec.Command("codesign", "--verify", "--strict", executable).Run(); err != nil {
		return fmt.Errorf("verify Vibe Remote signature: %w", err)
	}
	return writeFileAtomic(identityPath, []byte(identity+"\n"), 0o600)
}
