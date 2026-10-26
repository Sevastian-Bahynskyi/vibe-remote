package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/model"
	"github.com/Sevastian-Bahynskyi/vibe-remote/internal/store"
)

func runSetupJob(repository *store.Store, id string, operation func(context.Context) (model.Account, error)) (model.Account, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	if id == "" {
		return operation(ctx)
	}
	job, err := repository.GetSetupJob(ctx, id)
	if err != nil || job.State != model.SetupPending {
		return model.Account{}, errors.New("setup is no longer pending")
	}
	if err := repository.TouchSetupJob(ctx, id); err != nil {
		return model.Account{}, err
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	defer func() { close(done); <-stopped }()
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				job, err := repository.GetSetupJob(ctx, id)
				if err != nil || job.State != model.SetupPending {
					cancel()
					return
				}
				if err := repository.TouchSetupJob(ctx, id); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	account, err := operation(ctx)
	state := model.SetupReady
	if err != nil {
		state = model.SetupFailed
		if ctx.Err() != nil {
			state = model.SetupCancelled
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	finishErr := repository.FinishSetupJob(finishCtx, id, account.ID, state)
	if err == nil {
		err = finishErr
	}
	return account, err
}
