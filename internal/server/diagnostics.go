package server

import (
	"errors"
	"net/http"
	"runtime"
)

func (s *Server) diagnostics(response http.ResponseWriter, request *http.Request) {
	if !consoleRequestAllowed(request) {
		writeError(response, errors.New("diagnostics require the private dashboard"), http.StatusForbidden)
		return
	}
	stored, err := s.store.Counters(request.Context())
	if err != nil {
		writeError(response, errors.New("diagnostics are unavailable"), http.StatusInternalServerError)
		return
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	writeJSON(response, http.StatusOK, map[string]any{"heapAllocBytes": memory.HeapAlloc, "heapObjects": memory.HeapObjects, "goroutines": runtime.NumGoroutine(), "workers": s.claude.ResourceCounters(), "store": stored, "requests": s.requestCount.Load()})
}

func (s *Server) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		s.requestCount.Add(1)
		next.ServeHTTP(response, request)
	})
}
