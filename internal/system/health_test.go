package system

import (
	"errors"
	"testing"
)

func TestParseTailscaleExposureTailnetOnly(t *testing.T) {
	t.Parallel()
	status := "https://host.example.ts.net (tailnet only)\n|-- /vibe-remote proxy http://127.0.0.1:47173\n"
	serveReady, funnelOff := parseTailscaleExposure(status, nil, status, nil, "http://127.0.0.1:47173")
	if !serveReady || !funnelOff {
		t.Fatalf("expected tailnet-only route, got serveReady=%v funnelOff=%v", serveReady, funnelOff)
	}
}

func TestParseTailscaleExposureRejectsFunnel(t *testing.T) {
	t.Parallel()
	serve := "https://host.example.ts.net (tailnet only)\n|-- /vibe-remote proxy http://127.0.0.1:47173\n"
	funnel := "Available on the internet:\nhttps://host.example.ts.net\n|-- /vibe-remote proxy http://127.0.0.1:47173\n"
	serveReady, funnelOff := parseTailscaleExposure(serve, nil, funnel, nil, "http://127.0.0.1:47173")
	if !serveReady || funnelOff {
		t.Fatalf("expected Serve ready and Funnel active, got serveReady=%v funnelOff=%v", serveReady, funnelOff)
	}
}

func TestParseTailscaleExposureRejectsMissingOrFailedStatus(t *testing.T) {
	t.Parallel()
	status := "https://host.example.ts.net (tailnet only)\n|-- /other proxy http://127.0.0.1:9999\n"
	serveReady, funnelOff := parseTailscaleExposure(status, nil, "", errors.New("status failed"), "http://127.0.0.1:47173")
	if serveReady || funnelOff {
		t.Fatalf("expected exposure checks to fail closed, got serveReady=%v funnelOff=%v", serveReady, funnelOff)
	}
}
