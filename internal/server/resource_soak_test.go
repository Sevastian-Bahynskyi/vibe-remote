package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/claude"
)

func TestServeResourceSoak(t *testing.T) {
	address := os.Getenv("SOAK_ADDR")
	if address == "" {
		t.Skip("SOAK_ADDR enables the disposable resource fixture")
	}
	service, db := newTestServerWithStore(t, inertRunner{})
	script := filepath.Join(t.TempDir(), "claude-synthetic")
	data := []byte(`#!/bin/sh
if [ "$1" = auth ]; then printf '{"loggedIn":true}\n'; exit 0; fi
sleep 2
printf 'https://claude.ai/code/synthetic-resource-fixture\r\n'
i=0
while :; do
  n=0
  while [ "$n" -lt 100 ]; do
    printf 'VIBE_REMOTE_SYNTHETIC_MARKER %0100d\r\n' "$i"
    i=$((i+1)); n=$((n+1))
  done
  sleep 0.2
done
`)
	if err := os.WriteFile(script, data, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := claude.New(service.layout.Root, db, claude.Options{ClaudePath: script, BaselineSource: "-", StopTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	service.claude = manager
	seedRunnableSlot(t, service, db, "synthetic", "11111111-2222-3333-4444-555555555555")
	remote, err := db.GetRemoteSession(t.Context(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Activate(t.Context(), workerSpec(remote)); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: service.http.Handler}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	t.Log("Synthetic dashboard ready at http://" + address)
	select {
	case <-t.Context().Done():
	case <-time.After(50 * time.Minute):
	}
}
