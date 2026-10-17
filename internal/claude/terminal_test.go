package claude

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func TestTerminalReplayIsBoundedAndPreservesRawBytes(t *testing.T) {
	t.Parallel()
	buffer := newTerminalBuffer()
	input := append(bytes.Repeat([]byte("x"), terminalBufferLimit+17), []byte("\x1b[2JAllow Notes?\r\n")...)
	_, _ = buffer.Write(input)
	chunk := buffer.read(0)
	if chunk.Start != uint64(len(input)-terminalBufferLimit) || len(chunk.Data) != terminalChunkLimit || chunk.Total != uint64(len(input)) {
		t.Fatalf("unexpected replay bounds: start=%d length=%d total=%d", chunk.Start, len(chunk.Data), chunk.Total)
	}
	var replay []byte
	for cursor := chunk.Start; cursor < chunk.Total; {
		next := buffer.read(cursor)
		replay = append(replay, next.Data...)
		cursor = next.End
	}
	if !bytes.Equal(replay, input[len(input)-terminalBufferLimit:]) {
		t.Fatal("terminal replay changed the raw byte stream")
	}
	_, _ = buffer.Write([]byte("new output"))
	if next := buffer.read(chunk.Total); string(next.Data) != "new output" {
		t.Fatal("incremental replay lost output")
	}
	replay[0] = 0
	if buffer.read(0).Data[0] == 0 {
		t.Fatal("reader can modify the shared replay buffer")
	}
}

type consoleTestProcess struct {
	*fakeProcess
	buffer  *terminalBuffer
	input   []byte
	columns uint16
	rows    uint16
}

func (p *consoleTestProcess) readTerminal(cursor uint64) TerminalChunk { return p.buffer.read(cursor) }
func (p *consoleTestProcess) writeTerminal(data []byte) error {
	p.input = append(p.input, data...)
	return nil
}
func (p *consoleTestProcess) resizeTerminal(columns, rows uint16) error {
	p.columns, p.rows = columns, rows
	return nil
}

func TestConsoleInputCannotReachAReplacementWorker(t *testing.T) {
	t.Parallel()
	p := &consoleTestProcess{fakeProcess: newFakeProcess(1, true, true), buffer: newTerminalBuffer()}
	_, _ = p.buffer.Write([]byte("Approve Notes"))
	m := &Manager{workers: map[string]*worker{"slot": {process: p, ctx: context.Background(), activityRunID: "new-run"}}}
	chunk, err := m.ReadTerminal("slot", 0)
	if err != nil || chunk.RunID != "new-run" || string(chunk.Data) != "Approve Notes" {
		t.Fatalf("read terminal failed: %v", err)
	}
	if err := m.UpdateTerminal("slot", "old-run", []byte("\r"), 0, 0); !errors.Is(err, ErrTerminalChanged) {
		t.Fatalf("stale input error = %v", err)
	}
	if len(p.input) != 0 {
		t.Fatal("stale input reached the replacement worker")
	}
	if err := m.UpdateTerminal("slot", "new-run", []byte("\x1b[B"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTerminal("slot", "new-run", nil, 80, 30); err != nil {
		t.Fatal(err)
	}
	if string(p.input) != "\x1b[B" || p.columns != 80 || p.rows != 30 {
		t.Fatal("console did not reach the selected worker")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	m.workers["slot"].ctx = cancelled
	if err := m.UpdateTerminal("slot", "new-run", []byte("\r"), 0, 0); !errors.Is(err, ErrTerminalUnavailable) {
		t.Fatal("stopping worker accepted input")
	}
}

func TestTerminalBufferConcurrentReadersAndWriter(t *testing.T) {
	t.Parallel()
	buffer := newTerminalBuffer()
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Go(func() {
			for j := 0; j < 100; j++ {
				_, _ = buffer.Write([]byte("\x1b[2Jprompt"))
				chunk := buffer.read(0)
				if chunk.End > chunk.Total {
					t.Error("cursor advanced beyond output")
				}
			}
		})
	}
	group.Wait()
}
