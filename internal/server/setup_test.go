package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func TestAccountSetupResponsesCancellationAndRepair(t *testing.T) {
	server, db := newTestServerWithStore(t, inertRunner{})
	launched := 0
	server.launchSetup = func(email, accountID, jobID string) error {
		launched++
		if jobID == "" {
			t.Fatal("missing durable job")
		}
		return nil
	}
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"email":"fixture@example.com"}`))
	request.Header.Set("X-Vibe-Remote", "1")
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	var added struct {
		JobID string         `json:"jobId"`
		Job   model.SetupJob `json:"job"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.JobID == "" || added.JobID != added.Job.ID || launched != 1 {
		t.Fatal("setup response lost correlation")
	}
	request = httptest.NewRequest(http.MethodPost, "/api/setup-jobs/"+added.JobID+"/cancel", nil)
	request.Header.Set("X-Vibe-Remote", "1")
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if err := db.FinishSetupJob(context.Background(), added.JobID, "", model.SetupReady); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetSetupJob(context.Background(), added.JobID)
	if job.State != model.SetupCancelled {
		t.Fatal("late success replaced cancel")
	}
	request = httptest.NewRequest(http.MethodPost, "/api/setup-jobs/"+added.JobID+"/retry", nil)
	request.Header.Set("X-Vibe-Remote", "1")
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || launched != 2 {
		t.Fatal("retry did not start a new owned operation")
	}
	account, err := db.UpsertAccount(context.Background(), model.Account{ID: "repair-account", Email: "repair@example.com", Status: model.AccountAuthenticated})
	if err != nil {
		t.Fatal(err)
	}
	server.launchSetup = func(email, accountID, jobID string) error {
		if accountID != account.ID {
			t.Fatal("repair lost account")
		}
		return errors.New("fixture sensitive helper error")
	}
	request = httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/repair", nil)
	request.Header.Set("X-Vibe-Remote", "1")
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "sensitive") {
		t.Fatal("helper failure was not sanitized")
	}
	jobs, _ := db.ListSetupJobs(context.Background())
	found := false
	for _, job := range jobs {
		if job.AccountID == account.ID {
			found = true
			if job.State != model.SetupFailed {
				t.Fatal("repair failure not durable")
			}
		}
	}
	if !found {
		t.Fatal("repair job missing")
	}
}
