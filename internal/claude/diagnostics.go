package claude

import (
	"context"
	"time"
)

type ResourceCounters struct {
	Workers             int `json:"workers"`
	TerminalBufferBytes int `json:"terminalBufferBytes"`
}

func (m *Manager) ResourceCounters() ResourceCounters {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var value ResourceCounters
	for _, worker := range m.workers {
		if worker.process == nil || !worker.live() {
			continue
		}
		value.Workers++
		if process, ok := worker.process.(*execProcess); ok {
			process.console.mu.Lock()
			value.TerminalBufferBytes += len(process.console.data)
			process.console.mu.Unlock()
		}
	}
	return value
}

func (m *Manager) endFailedActivity(w *worker) {
	if w.activityRunID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = m.store.EndWorkerActivity(ctx, w.id, w.activityRunID)
}
