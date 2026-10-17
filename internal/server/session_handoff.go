package server

import (
	"context"
	"net/http"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
)

func (s *Server) handoffSlotOp(ctx context.Context, id string, provider model.Provider, accountID string, force bool) (model.Handoff, string, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	remote, err := s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.Handoff{}, "", failText(http.StatusNotFound, "That session no longer exists.")
	}
	if provider != model.ProviderClaude && provider != model.ProviderCodex {
		return model.Handoff{}, "", failText(http.StatusBadRequest, "Choose Codex or another Claude account.")
	}
	if provider == model.ProviderClaude {
		account, accountErr := s.store.GetAccount(ctx, accountID)
		if accountErr != nil || account.Status != model.AccountAuthenticated || account.ID == remote.AccountID {
			return model.Handoff{}, "", failText(http.StatusBadRequest, "Choose another signed-in Claude account.")
		}
	} else if accountID != "" {
		return model.Handoff{}, "", failText(http.StatusBadRequest, "Codex does not use a Claude account.")
	}
	source, err := s.store.GetSessionByNativeID(ctx, model.ProviderClaude, remote.ResumeSessionID)
	if err != nil || source.AccountID != remote.AccountID || source.WorkspacePath != remote.WorkspacePath {
		return model.Handoff{}, "", failText(http.StatusConflict, "Send a message in this session before handing it off.")
	}
	if err := s.stopRemote(ctx, remote, force); err != nil {
		return model.Handoff{}, "", failStop(err)
	}
	if _, err := s.store.SetRemoteSessionDesired(ctx, id, model.DesiredStopped); err != nil {
		return model.Handoff{}, "", failText(http.StatusConflict, "Could not save the stopped session. Try again.")
	}
	remote, err = s.store.GetRemoteSession(ctx, id)
	if err != nil {
		return model.Handoff{}, "", failText(http.StatusConflict, "Could not read the stopped session. Try again.")
	}
	source, err = s.store.GetSessionByNativeID(ctx, model.ProviderClaude, remote.ResumeSessionID)
	if err != nil || source.AccountID != remote.AccountID || source.WorkspacePath != remote.WorkspacePath {
		return model.Handoff{}, "", failText(http.StatusConflict, "Could not read the latest conversation. Try again.")
	}
	return s.createHandoffLocked(ctx, source.ID, provider, accountID)
}

func (s *Server) uiSessionHandoff(response http.ResponseWriter, request *http.Request) {
	if err := parseForm(response, request); err != nil {
		s.renderFragment(response, http.StatusBadRequest, "notice-oob", noticeFor(err))
		return
	}
	id := request.PathValue("id")
	provider := model.Provider(field(request, "destinationProvider"))
	accountID := field(request, "destinationAccountId")
	handoff, destination, err := s.handoffSlotOp(request.Context(), id, provider, accountID, field(request, "force") == "1")
	if err != nil {
		s.failSessionAction(response, request, id, err, "ui/sessions/"+id+"/handoff", "Stop and hand off",
			[]FieldView{{Name: "destinationProvider", Value: string(provider)}, {Name: "destinationAccountId", Value: accountID}})
		return
	}
	if provider == model.ProviderClaude {
		response.Header().Set("HX-Redirect", screenURL("session", handoff.ID))
		response.WriteHeader(http.StatusOK)
		return
	}
	s.renderSessionOutcome(response, request, http.StatusOK, id,
		NoticeView{Message: handoffInstructions(provider, destination), Tone: ToneGood}, nil)
}

func (s *Server) uiFragmentHandoff(response http.ResponseWriter, request *http.Request) {
	detail, err := s.sessionDetail(request.Context(), request.PathValue("id"), isLocalRequest(request), nil)
	if err != nil {
		s.renderFragment(response, statusOf(err), "notice-oob", noticeFor(err))
		return
	}
	s.renderFragment(response, http.StatusOK, "session-handoff", detail)
}
