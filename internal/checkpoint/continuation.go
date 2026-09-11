package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const ContinuationPrompt = "Resume the work from the attached Vibe Remote checkpoint."

func continuationPath(root, slotID string) (string, error) {
	if slotID == "" || strings.ContainsAny(slotID, `/\\`) || slotID == "." || slotID == ".." {
		return "", errors.New("invalid continuation slot")
	}
	return filepath.Join(root, "continuations", slotID), nil
}

func SaveContinuation(root, slotID, text string) error {
	path, err := continuationPath(root, slotID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func LoadContinuation(root, slotID string) (string, error) {
	path, err := continuationPath(root, slotID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

func RemoveContinuation(root, slotID string) error {
	path, err := continuationPath(root, slotID)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
