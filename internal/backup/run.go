package backup

import (
	"context"
	"errors"
	"fmt"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kyyard-server/internal/config"
	"github.com/Busnes-app/kyyard-server/internal/store"
)

// lastAttemptKey is the row recoveryclient.Run stamps before it works and NextRun counts from.
// The lib does not export it; TestInterruptedRunIsDueAgain fails if the two drift apart.
const lastAttemptKey = "backup_last_attempt"

// ErrInterrupted marks a run cut short because the server is stopping.
var ErrInterrupted = errors.New("backup: interrupted by shutdown")

// Run is recoveryclient.Run for a process that may be told to stop mid-run. Cancelling ctx
// aborts the snapshot and the upload. The settings writes ignore the cancellation, so a
// receipt KyRecovery has already returned is still recorded. An interrupted run gives its
// attempt stamp back, so the scheduler runs it again on the next start, not an interval later.
func Run(ctx context.Context, cfg *config.Config, rc recoveryclient.RunConfig, st store.SettingsStore, client recoveryclient.Depositor) (recoveryclient.Result, error) {
	settings := Settings(context.WithoutCancel(ctx), st)
	prev, prevErr := settings.Get(lastAttemptKey)
	res, err := recoveryclient.Run(ctx, rc, settings, func() (recoveryclient.Payload, error) {
		return Collect(ctx, cfg, rc.AppVersion)
	}, client)
	// ErrInProgress stamped nothing: the stamp belongs to the run that holds the lock.
	if err == nil || ctx.Err() == nil || res.Receipt != nil || errors.Is(err, recoveryclient.ErrInProgress) {
		return res, err
	}
	switch {
	case errors.Is(prevErr, recoveryclient.ErrNotFound):
		_ = settings.Delete(lastAttemptKey)
	case prevErr == nil:
		_ = settings.Set(lastAttemptKey, prev)
	}
	return res, fmt.Errorf("%w: %w", ErrInterrupted, err)
}
