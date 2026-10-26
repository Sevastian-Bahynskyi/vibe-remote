package claude

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/hinshun/vt10x"
)

type activityReader interface {
	WorkerActivity(context.Context, string, string) (model.WorkerActivity, error)
}

func (m *Manager) computerUseRequest(w *worker, runID string) func() string {
	return func() string {
		reader, ok := m.store.(activityReader)
		if !ok || w.ctx.Err() != nil {
			return ""
		}
		m.mu.RLock()
		current := m.workers[w.id] == w && w.activityRunID == runID
		m.mu.RUnlock()
		if !current {
			return ""
		}
		ctx, cancel := context.WithTimeout(w.ctx, time.Second)
		defer cancel()
		activity, err := reader.WorkerActivity(ctx, w.id, runID)
		if err != nil {
			return ""
		}
		for _, tool := range activity.Tools {
			if strings.HasPrefix(tool.Name, "mcp__computer-use__") {
				return runID + ":" + tool.Name + ":" + tool.StartedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		return ""
	}
}

type computerUseApprover struct {
	mu        sync.Mutex
	screen    vt10x.Terminal
	pending   []byte
	process   *execProcess
	request   func() string
	candidate string
	stableAt  time.Time
	navigated string
	answered  string
}

func newComputerUseApprover(process *execProcess, request func() string) *computerUseApprover {
	return &computerUseApprover{screen: vt10x.New(vt10x.WithSize(80, 30)), process: process, request: request}
}

func (a *computerUseApprover) Write(data []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, data...)
	length := 0
	for length < len(a.pending) && utf8.FullRune(a.pending[length:]) {
		_, size := utf8.DecodeRune(a.pending[length:])
		length += size
	}
	if length > 0 {
		_, _ = a.screen.Write(a.pending[:length])
		a.pending = append(a.pending[:0], a.pending[length:]...)
	}
	return len(data), nil
}

func (a *computerUseApprover) resize(columns, rows int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.screen.Resize(columns, rows)
	a.candidate = ""
}

func (a *computerUseApprover) run() {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-a.process.done:
			return
		case <-timer.C:
			if a.approve() {
				timer.Reset(200 * time.Millisecond)
			} else {
				timer.Reset(2 * time.Second)
			}
		}
	}
}

func (a *computerUseApprover) approve() bool {
	request := a.request()
	if request == "" {
		a.mu.Lock()
		a.candidate = ""
		a.mu.Unlock()
		return false
	}
	a.mu.Lock()
	screen := a.screen.String()
	plain := strings.Join(strings.Fields(strings.ReplaceAll(screen, "│", " ")), " ")
	_, rows := a.screen.Size()
	if request != "" && rows < 60 && strings.Contains(plain, "not shown, and not included in this grant") {
		a.mu.Unlock()
		_ = a.process.resizeTerminal(120, 60)
		return true
	}
	defer a.mu.Unlock()

	key := computerUseApprovalKey(screen)
	if key == "" {
		a.candidate = ""
		return true
	}
	candidate := request + ":" + key
	if candidate != a.candidate {
		a.candidate = candidate
		a.stableAt = time.Now()
		return true
	}
	if time.Since(a.stableAt) < 350*time.Millisecond || request == a.answered || (key != "\r" && request == a.navigated) {
		return true
	}
	if err := a.process.writeTerminal([]byte(key)); err != nil {
		return true
	}
	if key == "\r" {
		a.answered = request
	} else {
		a.navigated = request
	}
	return true
}

var computerUseAllow = regexp.MustCompile(`Allow for this session \([1-9][0-9]* apps?\)`)

func computerUseApprovalKey(screen string) string {
	plain := strings.Join(strings.Fields(strings.ReplaceAll(screen, "│", " ")), " ")
	if strings.Count(plain, "Computer Use wants to control these apps") != 1 ||
		strings.Contains(plain, "not shown, and not included in this grant") ||
		!strings.Contains(plain, "Deny, and tell Claude what to do differently") ||
		len(computerUseAllow.FindAllString(plain, -1)) != 1 {
		return ""
	}
	for _, line := range strings.Split(screen, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "│ ")
		if !strings.HasPrefix(line, "❯ ") {
			continue
		}
		option := strings.TrimSpace(strings.TrimPrefix(line, "❯ "))
		if strings.HasPrefix(option, "Allow for this session") {
			return "\r"
		}
		if strings.HasPrefix(option, "Deny, and tell Claude what to do differently") {
			return "\x1b[B"
		}
	}
	return ""
}
