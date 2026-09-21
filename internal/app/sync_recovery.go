package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const (
	// appStateRecoveryStepTimeout bounds each repair step: taking the chat-state
	// lock, waiting for the primary device's recovery snapshot, and the full
	// replay. The snapshot depends on the phone waking up and answering, which
	// can take well over 30 seconds on a locked device.
	appStateRecoveryStepTimeout = 2 * time.Minute
	// appStateRecoveryRetryDelay is how long a failed repair sequence defers the
	// next automatic one for the same collection. It replaces the old
	// once-per-run budget: a successful repair admits the next mismatch right
	// away, so a collection that receives patches all day stays at the server
	// head, while a failing one cannot request a phone snapshot on every patch.
	appStateRecoveryRetryDelay = 2 * time.Minute
)

func (a *App) handleAppStateSyncError(ctx context.Context, evt *events.AppStateSyncError, recoveries *sync.Map) {
	if evt == nil || !errors.Is(evt.Error, appstate.ErrMismatchingLTHash) {
		return
	}
	if a.ownsManualAppStateFetch(evt.Name) {
		return
	}
	name := strings.TrimSpace(string(evt.Name))
	if name == "" {
		return
	}
	if recoveries == nil {
		recoveries = &sync.Map{}
	}
	a.appStateRecoveryMu.Lock()
	defer a.appStateRecoveryMu.Unlock()
	if a.appStateRecoveryClosing {
		return
	}
	if retryAt, deferred := a.appStateRecoveryRetryAt[name]; deferred && nowUTC().Before(retryAt) {
		return
	}
	if _, loaded := recoveries.LoadOrStore(name, struct{}{}); loaded {
		return
	}

	a.appStateRecoveryWorkers.Go(func() {
		a.emitWarning("app_state_lthash_mismatch",
			fmt.Sprintf("warning: app state %s hit an LTHash mismatch; requesting recovery snapshot", name),
			map[string]any{"name": name})
		a.recoverAppStateCollection(ctx, name, recoveries, appStateRecoveryStepTimeout, true)
	})
}

// recoverAppStateCollection repairs one collection under the chat-state lock.
//
// snapshotFirst selects the order of the two repair steps. A mismatch reported
// by whatsmeow asks the primary device for a recovery snapshot first: the
// snapshot restores the collection at the server head without replaying the
// patch chain, so it also works on an account whose chain never verifies. A
// full replay is the fallback for when the phone does not answer.
//
// A pending recovery intent found at startup keeps the opposite order. The
// intent means local persistence may have missed events, not that the hash
// state diverged, and a full replay re-derives local state from the server
// without waking the phone.
func (a *App) recoverAppStateCollection(ctx context.Context, name string, recoveries *sync.Map, timeout time.Duration, snapshotFirst bool) {
	repaired := false
	defer func() {
		// Release the in-flight guard either way. A failed sequence defers the
		// next automatic attempt instead of blocking it for the rest of the run,
		// and a cancelled one leaves nothing behind.
		recoveries.Delete(name)
		if ctx.Err() == nil {
			a.setAppStateRecoveryOutcome(name, repaired)
		}
	}()
	lockCtx, cancelLock := context.WithTimeout(ctx, timeout)
	release, err := a.acquireChatStateSync(lockCtx)
	cancelLock()
	if err != nil {
		a.warnAppStateRecovery(name, err)
		return
	}
	defer release()

	generation, _, err := a.db.BeginAppStateRecovery(name)
	if err != nil {
		a.warnAppStateRecovery(name, err)
		return
	}
	collection := appstate.WAPatchName(name)
	tracker := &appStatePersistenceTracker{}

	if snapshotFirst {
		err = a.recoverAppStateSnapshot(ctx, name, collection, generation, tracker, timeout)
		if err == nil {
			repaired = true
			return
		}
		if ctx.Err() != nil || isAppStatePersistenceFailure(err) {
			a.warnAppStateRecovery(name, err)
			return
		}
		a.emitWarning("app_state_recovery_snapshot_failed",
			fmt.Sprintf("warning: app state %s recovery snapshot failed: %v; falling back to full sync", name, err),
			map[string]any{"name": name, "error": err.Error()})
		if err = a.replayAppStateFull(ctx, name, collection, generation, tracker, timeout); err != nil {
			a.warnAppStateRecovery(name, err)
			return
		}
		repaired = true
		return
	}

	err = a.replayAppStateFull(ctx, name, collection, generation, tracker, timeout)
	if err == nil {
		repaired = true
		return
	}
	if ctx.Err() != nil || isAppStatePersistenceFailure(err) {
		a.warnAppStateRecovery(name, err)
		return
	}
	a.emitWarning("app_state_full_sync_failed",
		fmt.Sprintf("warning: app state %s full sync failed: %v; requesting recovery snapshot", name, err),
		map[string]any{"name": name, "error": err.Error()})
	if err = a.recoverAppStateSnapshot(ctx, name, collection, generation, tracker, timeout); err != nil {
		a.warnAppStateRecovery(name, err)
		return
	}
	repaired = true
}

// recoverAppStateSnapshot asks the primary device for a snapshot of one
// collection and waits, bounded by timeout, for whatsmeow to apply it.
func (a *App) recoverAppStateSnapshot(ctx context.Context, name string, collection appstate.WAPatchName, generation int64, tracker *appStatePersistenceTracker, timeout time.Duration) error {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := nowUTC()
	err := a.recoverMismatchingAppState(stepCtx, collection, generation, tracker, func(id types.MessageID) {
		if a.eventsEnabled() {
			a.emitEvent("app_state_recovery_requested", map[string]any{"name": name, "id": string(id)})
		} else {
			fmt.Fprintf(os.Stderr, "\rRequested app state %s recovery (id %s)\n", name, id)
		}
	})
	if err != nil {
		return err
	}
	elapsed := nowUTC().Sub(started).Round(time.Millisecond)
	a.emitOrPrint("app_state_recovery_completed", map[string]any{"name": name, "elapsed_ms": elapsed.Milliseconds()},
		"\rApp state %s resolved via recovery snapshot in %s\n", name, elapsed)
	return nil
}

// replayAppStateFull re-fetches one collection from its server snapshot,
// bounded by timeout, and clears the recovery intent when the replay verifies.
func (a *App) replayAppStateFull(ctx context.Context, name string, collection appstate.WAPatchName, generation int64, tracker *appStatePersistenceTracker, timeout time.Duration) error {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fetchErr, persistenceErr := a.fetchAndPersistAppState(stepCtx, collection, true, tracker)
	if persistenceErr != nil {
		return &appStatePersistenceFailure{err: fmt.Errorf("persist full app state replay: %w", persistenceErr)}
	}
	if fetchErr != nil {
		return fetchErr
	}
	if err := a.clearCompletedAppStateRecovery(collection, generation); err != nil {
		return err
	}
	a.emitOrPrint("app_state_full_sync_completed", map[string]any{"name": name},
		"\rApp state %s resolved via full sync\n", name)
	return nil
}

// setAppStateRecoveryOutcome records whether the last automatic repair of a
// collection succeeded. A failure defers the next automatic attempt.
func (a *App) setAppStateRecoveryOutcome(name string, repaired bool) {
	a.appStateRecoveryMu.Lock()
	defer a.appStateRecoveryMu.Unlock()
	if repaired {
		delete(a.appStateRecoveryRetryAt, name)
		return
	}
	if a.appStateRecoveryRetryAt == nil {
		a.appStateRecoveryRetryAt = make(map[string]time.Time)
	}
	a.appStateRecoveryRetryAt[name] = nowUTC().Add(appStateRecoveryRetryDelay)
}

// appStatePersistenceFailure marks an error raised while writing fetched or
// recovered app state into the local database. The repair ladder does not try
// its other step after one: the second step persists through the same path.
type appStatePersistenceFailure struct{ err error }

func (e *appStatePersistenceFailure) Error() string { return e.err.Error() }
func (e *appStatePersistenceFailure) Unwrap() error { return e.err }

func isAppStatePersistenceFailure(err error) bool {
	var failure *appStatePersistenceFailure
	return errors.As(err, &failure)
}

func (a *App) warnAppStateRecovery(name string, err error) {
	a.emitWarning("app_state_recovery_failed",
		fmt.Sprintf("warning: app state %s recovery failed: %v", name, err),
		map[string]any{"name": name, "error": err.Error()})
}

func (a *App) syncAppStateDeltas(ctx context.Context, recoveries *sync.Map) {
	pending, err := a.db.AppStateRecoveryCollections()
	if err != nil {
		a.emitWarning("app_state_sync_failed", fmt.Sprintf("warning: cannot inspect app state recovery: %v", err),
			map[string]any{"error": err.Error()})
		return
	}
	replayed := make(map[string]bool, len(pending))
	for _, name := range pending {
		if _, loaded := recoveries.LoadOrStore(name, struct{}{}); !loaded {
			a.recoverAppStateCollection(ctx, name, recoveries, appStateRecoveryStepTimeout, false)
			replayed[name] = true
		}
	}
	for _, name := range []appstate.WAPatchName{appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow, appstate.WAPatchRegular} {
		// A collection replayed just now is already at the server head, and a
		// collection whose replay failed gains nothing from an incremental
		// fetch that would fail the same way.
		if _, recovering := recoveries.Load(string(name)); recovering || replayed[string(name)] {
			continue
		}
		// Never force a full sync here. whatsmeow deletes the stored version row
		// before a forced full sync (appstate.go:48), and the mutation-MAC table
		// cascades on that delete. On a collection whose patch chain fails to
		// verify, the replay then stops at the snapshot version, leaving the row
		// far behind the server head — and every subsequent write to that
		// collection is rejected with a 409 conflict until a recovery snapshot
		// restores the head.
		//
		// Forcing it buys nothing: fetchAppState promotes to a full sync by
		// itself when the stored version is 0 (appstate.go:58), so a collection
		// that was never synced still gets its snapshot.
		if err := a.wa.FetchAppState(ctx, string(name), false, false); err != nil {
			a.emitWarning("app_state_sync_failed",
				fmt.Sprintf("warning: failed to sync WhatsApp app state %s: %v", name, err),
				map[string]any{"name": string(name), "error": err.Error()})
		}
	}
}
