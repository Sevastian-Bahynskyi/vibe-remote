package claude

import (
	"errors"
	"sync"

	"github.com/creack/pty"
)

const terminalBufferLimit = 1 << 20
const terminalChunkLimit = 64 << 10

var ErrTerminalUnavailable = errors.New("approval console is unavailable; start the session and try again")
var ErrTerminalChanged = errors.New("session restarted; close and reopen the approval console")

type TerminalChunk struct {
	RunID string `json:"runId"`
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
	Total uint64 `json:"total"`
	Data  []byte `json:"data"`
}

type terminalProcess interface {
	readTerminal(uint64) TerminalChunk
	writeTerminal([]byte) error
	resizeTerminal(uint16, uint16) error
}

type terminalBuffer struct {
	mu   sync.Mutex
	data []byte
	end  uint64
}

func newTerminalBuffer() *terminalBuffer {
	return &terminalBuffer{}
}

func (b *terminalBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.end += uint64(len(data))
	if len(data) >= terminalBufferLimit {
		b.data = append(b.data[:0], data[len(data)-terminalBufferLimit:]...)
	} else {
		overflow := len(b.data) + len(data) - terminalBufferLimit
		if overflow > 0 {
			copy(b.data, b.data[overflow:])
			b.data = b.data[:len(b.data)-overflow]
		}
		b.data = append(b.data, data...)
	}
	return len(data), nil
}

func (b *terminalBuffer) read(cursor uint64) TerminalChunk {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := b.end - uint64(len(b.data))
	if cursor < start || cursor > b.end {
		cursor = start
	}
	offset := int(cursor - start)
	length := min(len(b.data)-offset, terminalChunkLimit)
	return TerminalChunk{Start: cursor, End: cursor + uint64(length), Total: b.end, Data: append([]byte(nil), b.data[offset:offset+length]...)}
}

func (p *execProcess) readTerminal(cursor uint64) TerminalChunk {
	return p.console.read(cursor)
}

func (p *execProcess) writeTerminal(data []byte) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()
	_, err := p.terminal.Write(data)
	return err
}

func (p *execProcess) resizeTerminal(columns, rows uint16) error {
	if p.approval != nil {
		p.approval.resize(int(columns), int(rows))
	}
	return pty.Setsize(p.terminal, &pty.Winsize{Cols: columns, Rows: rows})
}

func (m *Manager) ReadTerminal(slotID string, cursor uint64) (TerminalChunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w := m.workers[slotID]
	if w == nil || w.process == nil || w.ctx.Err() != nil {
		return TerminalChunk{}, ErrTerminalUnavailable
	}
	p, ok := w.process.(terminalProcess)
	if !ok {
		return TerminalChunk{}, ErrTerminalUnavailable
	}
	chunk := p.readTerminal(cursor)
	chunk.RunID = w.activityRunID
	return chunk, nil
}

func (m *Manager) UpdateTerminal(slotID, runID string, data []byte, columns, rows uint16) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w := m.workers[slotID]
	if w == nil || w.process == nil || w.ctx.Err() != nil {
		return ErrTerminalUnavailable
	}
	if runID == "" || runID != w.activityRunID {
		return ErrTerminalChanged
	}
	p, ok := w.process.(terminalProcess)
	if !ok {
		return ErrTerminalUnavailable
	}
	if len(data) > 0 {
		if err := p.writeTerminal(data); err != nil {
			return ErrTerminalUnavailable
		}
		return nil
	}
	if err := p.resizeTerminal(columns, rows); err != nil {
		return ErrTerminalUnavailable
	}
	return nil
}
