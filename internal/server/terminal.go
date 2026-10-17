package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var terminalKeys = map[string]string{
	"up": "\x1b[A", "down": "\x1b[B", "left": "\x1b[D", "right": "\x1b[C",
	"tab": "\t", "space": " ", "enter": "\r", "escape": "\x1b",
}

func consoleRequestAllowed(request *http.Request) bool {
	if request.Header.Get("X-Vibe-Remote") != "1" || request.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		host := request.Host
		if forwarded := request.Header.Get("X-Forwarded-Host"); forwarded != "" {
			host = forwarded
		}
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, host) {
			return false
		}
	}
	return true
}

func (s *Server) readTerminal(response http.ResponseWriter, request *http.Request) {
	if !consoleRequestAllowed(request) {
		writeError(response, errors.New("console access requires the Vibe Remote dashboard"), http.StatusForbidden)
		return
	}
	cursor := uint64(0)
	if value := request.URL.Query().Get("cursor"); value != "" {
		var err error
		cursor, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeError(response, errors.New("invalid console cursor"), http.StatusBadRequest)
			return
		}
	}
	chunk, err := s.claude.ReadTerminal(request.PathValue("id"), cursor)
	if err != nil {
		writeError(response, err, http.StatusConflict)
		return
	}
	writeJSON(response, http.StatusOK, chunk)
}

func (s *Server) updateTerminal(response http.ResponseWriter, request *http.Request) {
	if !consoleRequestAllowed(request) {
		writeError(response, errors.New("console access requires the Vibe Remote dashboard"), http.StatusForbidden)
		return
	}
	var input struct {
		RunID   string `json:"runId"`
		Key     string `json:"key"`
		Columns uint16 `json:"columns"`
		Rows    uint16 `json:"rows"`
	}
	request.Body = http.MaxBytesReader(response, request.Body, 1024)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(response, errors.New("invalid console request"), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		writeError(response, errors.New("invalid console request"), http.StatusBadRequest)
		return
	}
	key, known := terminalKeys[input.Key]
	resize := input.Key == "" && input.Columns >= 40 && input.Columns <= 160 && input.Rows >= 12 && input.Rows <= 60
	if (!known && !resize) || (known && (input.Columns != 0 || input.Rows != 0)) {
		writeError(response, errors.New("choose a console key or a valid display size"), http.StatusBadRequest)
		return
	}
	if err := s.claude.UpdateTerminal(request.PathValue("id"), input.RunID, []byte(key), input.Columns, input.Rows); err != nil {
		writeError(response, err, http.StatusConflict)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}
