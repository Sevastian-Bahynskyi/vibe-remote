package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func snapshotNativeExecutable(sourcePath, runtimeDir string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("open Claude runtime: %w", err)
	}
	defer source.Close()
	var header [4]byte
	if _, err := io.ReadFull(source, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return sourcePath, nil
		}
		return "", fmt.Errorf("read Claude runtime: %w", err)
	}
	switch binary.BigEndian.Uint32(header[:]) {
	case 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, 0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
	default:
		return sourcePath, nil
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind Claude runtime: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", fmt.Errorf("identify Claude runtime: %w", err)
	}
	digest := hash.Sum(nil)
	directory := filepath.Join(runtimeDir, hex.EncodeToString(digest))
	destination := filepath.Join(directory, "claude")
	if _, err := os.Lstat(destination); err == nil {
		return verifyRuntimeSnapshot(destination, digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect Claude runtime snapshot: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create Claude runtime directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".claude-*")
	if err != nil {
		return "", fmt.Errorf("create Claude runtime snapshot: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("rewind Claude runtime: %w", err)
	}
	hash.Reset()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), source)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("copy Claude runtime: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close Claude runtime snapshot: %w", closeErr)
	}
	if !bytes.Equal(hash.Sum(nil), digest) {
		return "", errors.New("Claude runtime changed while preparing the session; retry launch")
	}
	if err := os.Chmod(file.Name(), 0o500); err != nil {
		return "", fmt.Errorf("protect Claude runtime snapshot: %w", err)
	}
	if err := os.Link(file.Name(), destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return verifyRuntimeSnapshot(destination, digest)
		}
		return "", fmt.Errorf("activate Claude runtime snapshot: %w", err)
	}
	return destination, nil
}

func verifyRuntimeSnapshot(path string, digest []byte) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect Claude runtime snapshot: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Claude runtime snapshot is not a regular executable")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open Claude runtime snapshot: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("verify Claude runtime snapshot: %w", err)
	}
	if !bytes.Equal(hash.Sum(nil), digest) {
		return "", errors.New("Claude runtime snapshot is damaged; remove it before retrying")
	}
	return path, nil
}
