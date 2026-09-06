package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

const (
	gitCommandTimeout = 600 * time.Millisecond
	maxGitStatusBytes = 64 * 1024
	maxChangedPaths   = 1000
)

type GitCapturer interface {
	Capture(context.Context, string) (model.GitSnapshot, error)
}

type Git struct {
	now func() time.Time
}

func NewGitCapturer() *Git {
	return &Git{now: time.Now}
}

func (g *Git) Capture(ctx context.Context, cwd string) (model.GitSnapshot, error) {
	snapshot := model.GitSnapshot{CapturedAt: g.now().UTC()}
	if strings.TrimSpace(cwd) == "" {
		return snapshot, nil
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return snapshot, err
	}

	root, err := gitOutput(ctx, absCWD, "rev-parse", "--show-toplevel")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return snapshot, nil
		}
		return snapshot, err
	}
	snapshot.Root = strings.TrimSpace(string(root))

	type commandResult struct {
		output []byte
		err    error
	}
	var branchResult commandResult
	var headResult commandResult
	var statusResult commandResult
	var porcelainResult commandResult
	commands := []struct {
		result *commandResult
		args   []string
	}{
		{result: &branchResult, args: []string{"symbolic-ref", "--short", "-q", "HEAD"}},
		{result: &headResult, args: []string{"rev-parse", "--verify", "HEAD"}},
		{result: &statusResult, args: []string{"status", "--porcelain=v2", "--branch", "--untracked-files=all"}},
		{result: &porcelainResult, args: []string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}},
	}
	var group sync.WaitGroup
	for index := range commands {
		command := &commands[index]
		group.Add(1)
		go func() {
			defer group.Done()
			command.result.output, command.result.err = gitOutput(ctx, snapshot.Root, command.args...)
		}()
	}
	group.Wait()

	branch, branchErr := branchResult.output, branchResult.err
	if branchErr == nil {
		snapshot.Branch = strings.TrimSpace(string(branch))
	} else {
		var exitErr *exec.ExitError
		if !errors.As(branchErr, &exitErr) {
			return snapshot, branchErr
		}
	}

	head, headErr := headResult.output, headResult.err
	if headErr == nil {
		snapshot.HeadSHA = strings.TrimSpace(string(head))
	} else {
		var exitErr *exec.ExitError
		if !errors.As(headErr, &exitErr) {
			return snapshot, headErr
		}
	}

	if statusResult.err != nil {
		return snapshot, statusResult.err
	}
	status := statusResult.output
	if len(status) > maxGitStatusBytes {
		status = status[:maxGitStatusBytes]
	}
	snapshot.Status = strings.TrimRight(string(status), "\n")

	if porcelainResult.err != nil {
		return snapshot, porcelainResult.err
	}
	snapshot.ChangedPaths = parseChangedPaths(porcelainResult.output)
	return snapshot, nil
}

func gitOutput(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, "git", append([]string{"-C", cwd}, args...)...)
	command.Stdin = nil
	var output bytes.Buffer
	command.Stdout = &limitedWriter{writer: &output, remaining: maxGitStatusBytes}
	err := command.Run()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	return output.Bytes(), err
}

type limitedWriter struct {
	writer    *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	original := len(data)
	if w.remaining > 0 {
		part := data
		if len(part) > w.remaining {
			part = part[:w.remaining]
		}
		_, _ = w.writer.Write(part)
		w.remaining -= len(part)
	}
	return original, nil
}

func parseChangedPaths(output []byte) []string {
	records := bytes.Split(output, []byte{0})
	paths := make(map[string]struct{})
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) < 4 {
			continue
		}
		status := string(record[:2])
		path := string(record[3:])
		if path != "" {
			paths[path] = struct{}{}
		}
		if (strings.Contains(status, "R") || strings.Contains(status, "C")) && index+1 < len(records) {
			index++
			originalPath := string(records[index])
			if originalPath != "" {
				paths[originalPath] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	if len(result) > maxChangedPaths {
		result = result[:maxChangedPaths]
	}
	return result
}
