package server

import (
	"context"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
	systemstate "github.com/Sevastian-Bahynskyi/vibe-remote/internal/system"
)

// maintenanceInterval is how often the retention sweep runs. It used to ride on
// the dashboard's poll of /api/state, which meant retention silently depended on
// somebody having the page open.
const maintenanceInterval = 6 * time.Hour

// dashboardState is one consistent read of what the dashboard shows, already
// decorated. The JSON encoder and the HTML view builders both consume it, so the
// two surfaces cannot disagree about what is running.
type dashboardState struct {
	RetentionDays int
	Accounts      []model.Account
	Workspaces    []model.Workspace
	Sessions      []model.Session
	Remotes       []model.RemoteSession
	// ConversationTitles maps a slot's native Claude conversation ID to the
	// checkpoint title for it, for the slots that resume one.
	ConversationTitles map[string]string
	Running            int
	Health             systemstate.Health
	PowerWarning       string
	OnThisMac          bool
	StartedAt          time.Time
}

// slotsSnapshot reads only what a session card needs. It is deliberately
// separate from snapshot: the dashboard refreshes this on a timer, and it must
// not pay for a 250-row checkpoint scan or for shelling out to tailscale.
func (s *Server) slotsSnapshot(ctx context.Context) (dashboardState, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return dashboardState{}, err
	}
	workspaces, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return dashboardState{}, err
	}
	remotes, err := s.store.ListRemoteSessions(ctx)
	if err != nil {
		return dashboardState{}, err
	}
	s.decorateRemoteSessions(remotes)
	decorateAccountActivity(accounts, remotes)
	return dashboardState{
		Accounts:           accounts,
		Workspaces:         workspaces,
		Remotes:            remotes,
		ConversationTitles: s.conversationTitles(ctx, remotes),
		Running:            s.claude.RunningCount(),
		StartedAt:          s.started,
	}, nil
}

// snapshot is slotsSnapshot plus the checkpoint list and system health.
func (s *Server) snapshot(ctx context.Context, local bool) (dashboardState, error) {
	state, err := s.slotsSnapshot(ctx)
	if err != nil {
		return dashboardState{}, err
	}
	sessions, err := s.store.ListSessions(ctx, store.SessionFilter{Limit: 250})
	if err != nil {
		return dashboardState{}, err
	}
	decorateSessions(sessions, state.Accounts)
	state.Sessions = sessions
	state.Health = s.health(ctx)
	if !state.Health.OnACPower {
		state.PowerWarning = "This Mac is on battery and may become unreachable."
	}
	state.OnThisMac = local
	state.RetentionDays, _ = s.retentionDays()
	return state, nil
}

// conversationTitles resolves the checkpoint title behind each slot's resumed
// conversation. Only the handful of conversations actually referenced are read,
// rather than scanning every checkpoint to label a few cards.
func (s *Server) conversationTitles(ctx context.Context, remotes []model.RemoteSession) map[string]string {
	titles := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		nativeID := remote.ResumeSessionID
		if nativeID == "" {
			continue
		}
		if _, seen := titles[nativeID]; seen {
			continue
		}
		session, err := s.store.GetSessionByNativeID(ctx, model.ProviderClaude, nativeID)
		if err != nil {
			continue
		}
		titles[nativeID] = session.Title
	}
	return titles
}

// health returns system health, re-inspecting at most once per healthTTL.
func (s *Server) health(ctx context.Context) systemstate.Health {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if !s.healthAt.IsZero() && time.Since(s.healthAt) < healthTTL {
		return s.healthValue
	}
	s.healthValue = systemstate.Inspect(ctx, s.layout.CodexHooks, s.layout.CodexHookVerified, s.layout.Binary)
	s.healthAt = time.Now()
	return s.healthValue
}

// StartMaintenance runs periodic housekeeping until ctx is cancelled. Checkpoint
// retention used to be a side effect of the dashboard polling /api/state; with
// the dashboard no longer polling that route it needs an owner that runs whether
// or not anyone has the page open.
func (s *Server) StartMaintenance(ctx context.Context) {
	go func() {
		s.sweepClosedSessions(ctx)
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepClosedSessions(ctx)
			}
		}
	}()
}

func (s *Server) sweepClosedSessions(ctx context.Context) {
	days, err := s.retentionDays()
	if err != nil {
		return
	}
	_, _ = s.store.CleanupClosedSessions(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour))
}
