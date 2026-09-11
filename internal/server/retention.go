package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *Server) retentionDays() (int, error) {
	data, err := os.ReadFile(filepath.Join(s.layout.Root, "checkpoint-retention-days"))
	if errors.Is(err, os.ErrNotExist) {
		return 7, nil
	}
	if err != nil {
		return 0, err
	}
	return parseRetentionDays(string(data))
}

func parseRetentionDays(value string) (int, error) {
	days, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || days < 1 || days > 3650 {
		return 0, errors.New("Choose a retention period between 1 and 3650 days.")
	}
	return days, nil
}

func (s *Server) saveRetentionDays(days int) error {
	file, err := os.CreateTemp(s.layout.Root, ".retention-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(strconv.Itoa(days) + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(s.layout.Root, "checkpoint-retention-days"))
}

func (s *Server) uiRetention(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderSystem(response, request, http.StatusBadRequest, noticeFor(err))
		return
	}
	days, err := parseRetentionDays(field(request, "days"))
	if err != nil {
		s.renderSystem(response, request, http.StatusBadRequest, noticeFor(err))
		return
	}
	if err := s.saveRetentionDays(days); err != nil {
		s.renderSystem(response, request, http.StatusInternalServerError, noticeFor(errors.New("Could not save checkpoint retention.")))
		return
	}
	s.sweepClosedSessions(request.Context())
	s.renderSystem(response, request, http.StatusOK, NoticeView{Message: "Retention saved. Old checkpoints have been cleaned up.", Tone: ToneGood})
}
