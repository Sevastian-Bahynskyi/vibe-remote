package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func (s *Server) startAccountSetup(ctx context.Context, email, accountID string) (model.SetupJob, error) {
	job, err := s.store.CreateSetupJob(ctx, email, accountID)
	if err != nil {
		return job, fail(http.StatusBadRequest, err)
	}
	err = s.launchSetup(email, accountID, job.ID)
	if err != nil {
		_ = s.store.FinishSetupJob(context.Background(), job.ID, accountID, model.SetupFailed)
		return job, failText(http.StatusInternalServerError, "Could not open setup on the Mac. Retry setup.")
	}
	return job, nil
}

func (s *Server) setupStatus(response http.ResponseWriter, request *http.Request) {
	job, err := s.store.GetSetupJob(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, err, http.StatusNotFound)
		return
	}
	writeJSON(response, http.StatusOK, job)
}

func (s *Server) setupAction(action string, html bool) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		job, err := s.store.GetSetupJob(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(response, err, http.StatusNotFound)
			return
		}
		if action == "cancel" {
			err = s.store.FinishSetupJob(request.Context(), job.ID, "", model.SetupCancelled)
		} else {
			accountID := job.AccountID
			if accountID == "" {
				accounts, listErr := s.store.ListAccounts(request.Context())
				if listErr != nil {
					err = listErr
				} else {
					for _, account := range accounts {
						if strings.EqualFold(account.Email, job.Email) {
							accountID = account.ID
							break
						}
					}
				}
			}
			if err == nil {
				job, err = s.startAccountSetup(request.Context(), job.Email, accountID)
			}
		}
		if html {
			s.renderAccounts(response, request, statusOf(err), noticeFor(err))
			return
		}
		if err != nil {
			writeError(response, err, statusOf(err))
			return
		}
		job, _ = s.store.GetSetupJob(request.Context(), job.ID)
		writeJSON(response, http.StatusOK, job)
	}
}

func (s *Server) repairAccount(html bool) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		account, err := s.store.GetAccount(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(response, err, http.StatusNotFound)
			return
		}
		job, err := s.startAccountSetup(request.Context(), account.Email, account.ID)
		if html {
			s.renderAccounts(response, request, statusOf(err), noticeFor(err))
			return
		}
		if err != nil {
			writeError(response, err, statusOf(err))
			return
		}
		writeJSON(response, http.StatusAccepted, map[string]any{"jobId": job.ID, "job": job})
	}
}

func (s *Server) accountsPanel(ctx context.Context, state dashboardState) (pageData, error) {
	jobs, err := s.store.ListSetupJobs(ctx)
	every := 20000
	for _, job := range jobs {
		if job.State == model.SetupPending {
			every = 1500
			break
		}
	}
	accounts := buildAccounts(state)
	for i := range accounts {
		report, reportErr := s.claude.ProfileReadiness(ctx, accounts[i].ID)
		accounts[i].SetupStatus = "Needs preparation or repair"
		if reportErr == nil && report.Ready {
			accounts[i].SetupStatus = "Shared setup ready"
		}
		accounts[i].SetupConflicts = len(report.Conflicts)
	}
	return pageData{Accounts: accounts, SetupJobs: jobs, SetupRefreshEvery: every}, err
}

func (s *Server) uiFragmentAccounts(response http.ResponseWriter, request *http.Request) {
	state, err := s.slotsSnapshot(request.Context())
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	data, err := s.accountsPanel(request.Context(), state)
	if err != nil {
		s.renderFragment(response, http.StatusInternalServerError, "notice-oob", noticeFor(err))
		return
	}
	s.renderFragment(response, http.StatusOK, "account-panel", data)
}

func (s *Server) monitorSetupJobs(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			_ = s.store.InterruptStaleSetupJobs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) accountReadiness(response http.ResponseWriter, request *http.Request) {
	report, err := s.claude.ProfileReadiness(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, err, http.StatusNotFound)
		return
	}
	writeJSON(response, http.StatusOK, report)
}
