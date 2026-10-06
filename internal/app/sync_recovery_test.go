package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type appStateContextWA struct {
	*fakeWA
	fetchAppState           func(context.Context, string, bool, bool) error
	fetchEvents             func(context.Context, string, bool, bool) ([]any, error)
	requestAppStateRecovery func(context.Context, string) (types.MessageID, error)
}

func (f *appStateContextWA) FetchAppState(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error {
	return f.fetchAppState(ctx, name, fullSync, onlyIfNotSynced)
}

func (f *appStateContextWA) FetchAppStateEvents(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) ([]any, error) {
	if f.fetchEvents != nil {
		return f.fetchEvents(ctx, name, fullSync, onlyIfNotSynced)
	}
	return nil, f.fetchAppState(ctx, name, fullSync, onlyIfNotSynced)
}

func (f *appStateContextWA) RequestAppStateRecovery(ctx context.Context, name string) (types.MessageID, error) {
	return f.requestAppStateRecovery(ctx, name)
}

func TestAppStateLTHashMismatchFullSyncGetsFreshTimeoutAfterSnapshotExpires(t *testing.T) {
	a := newTestApp(t)
	var fetchErr error
	var recoveryErr error
	fetchHasDeadline := false
	fetchCalls := 0
	f := &appStateContextWA{fakeWA: newFakeWA()}
	f.requestAppStateRecovery = func(ctx context.Context, name string) (types.MessageID, error) {
		<-ctx.Done()
		recoveryErr = ctx.Err()
		return "", recoveryErr
	}
	f.fetchAppState = func(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error {
		fetchCalls++
		fetchErr = ctx.Err()
		_, fetchHasDeadline = ctx.Deadline()
		return fetchErr
	}
	a.wa = f

	var recoveries sync.Map
	name := string(appstate.WAPatchRegularLow)
	recoveries.Store(name, struct{}{})
	a.recoverAppStateCollection(t.Context(), name, &recoveries, 10*time.Millisecond, true)

	if !errors.Is(recoveryErr, context.DeadlineExceeded) {
		t.Fatalf("snapshot context error = %v, want deadline exceeded", recoveryErr)
	}
	if fetchCalls != 1 {
		t.Fatalf("full sync calls = %d, want 1", fetchCalls)
	}
	if fetchErr != nil {
		t.Fatalf("full sync context was already expired: %v", fetchErr)
	}
	if !fetchHasDeadline {
		t.Fatal("full sync context has no timeout")
	}
}

func TestAppStateLTHashMismatchRecoveryRetainsParentCancellation(t *testing.T) {
	a := newTestApp(t)
	parentCtx, cancelParent := context.WithCancel(t.Context())
	defer cancelParent()
	recoveryStarted := make(chan struct{})
	var recoveryErr error
	var fetchCalls atomic.Int32
	f := &appStateContextWA{fakeWA: newFakeWA()}
	f.fetchAppState = func(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error {
		fetchCalls.Add(1)
		return errors.New("full sync failed")
	}
	f.requestAppStateRecovery = func(ctx context.Context, name string) (types.MessageID, error) {
		close(recoveryStarted)
		<-ctx.Done()
		recoveryErr = ctx.Err()
		return "", recoveryErr
	}
	a.wa = f

	var recoveries sync.Map
	name := string(appstate.WAPatchRegularLow)
	recoveries.Store(name, struct{}{})
	done := make(chan struct{})
	go func() {
		a.recoverAppStateCollection(parentCtx, name, &recoveries, 100*time.Millisecond, true)
		close(done)
	}()
	select {
	case <-recoveryStarted:
	case <-time.After(time.Second):
		t.Fatal("app state snapshot recovery did not start")
	}
	cancelParent()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("app state recovery did not stop after parent cancellation")
	}

	if !errors.Is(recoveryErr, context.Canceled) {
		t.Fatalf("recovery context error = %v, want parent cancellation", recoveryErr)
	}
	if got := fetchCalls.Load(); got != 0 {
		t.Fatalf("full sync fallback ran %d times after parent cancellation", got)
	}
	if _, loaded := recoveries.Load(name); loaded {
		t.Fatal("recovery guard remained set after parent cancellation")
	}
}

func TestAppStateLTHashMismatchRequestsSnapshotFirst(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.onAppStateRecovery = func(name string) {
		f.emit(&events.AppStateSyncComplete{Name: appstate.WAPatchName(name), Recovery: true})
	}
	a.wa = f

	var recoveries sync.Map
	err := fmt.Errorf("failed to verify patch v5848: %w", appstate.ErrMismatchingLTHash)
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name:  appstate.WAPatchRegularLow,
		Error: err,
	}, &recoveries)

	waitForCondition(t, time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.appStateRecoveries) == 1
	})
	a.appStateRecoveryWorkers.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.appStateRecoveries[0]; got != string(appstate.WAPatchRegularLow) {
		t.Fatalf("recovery collection = %q", got)
	}
	if len(f.appStateFetches) != 0 {
		t.Fatalf("full sync ran when the snapshot succeeded: %+v", f.appStateFetches)
	}
}

func TestAppStateLTHashMismatchFallsBackToFullSyncWhenSnapshotFails(t *testing.T) {
	a := newTestApp(t)
	var recoveryCalls atomic.Int32
	f := &appStateContextWA{fakeWA: newFakeWA()}
	f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
		recoveryCalls.Add(1)
		return "", errors.New("phone offline")
	}
	f.fetchAppState = func(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error {
		f.fakeWA.mu.Lock()
		defer f.fakeWA.mu.Unlock()
		f.appStateFetches = append(f.appStateFetches, fakeAppStateFetch{name: name, fullSync: fullSync, onlyIfNotSynced: onlyIfNotSynced})
		return nil
	}
	a.wa = f

	var recoveries sync.Map
	err := fmt.Errorf("failed to verify patch v5848: %w", appstate.ErrMismatchingLTHash)
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name:  appstate.WAPatchRegularLow,
		Error: err,
	}, &recoveries)
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name:  appstate.WAPatchRegularLow,
		Error: err,
	}, &recoveries)

	waitForCondition(t, time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.appStateFetches) == 1
	})
	a.appStateRecoveryWorkers.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.appStateFetches[0]; got.name != string(appstate.WAPatchRegularLow) || !got.fullSync {
		t.Fatalf("fallback fetch = %+v, want regular_low full sync", got)
	}
	if got := recoveryCalls.Load(); got != 1 {
		t.Fatalf("snapshot requests = %d, want 1 for two overlapping mismatches", got)
	}
}

func TestAppStateLTHashMismatchThrottlesAfterRecoveryFailure(t *testing.T) {
	a := newTestApp(t)
	var fetchCalls atomic.Int32
	var recoveryCalls atomic.Int32
	f := &appStateContextWA{fakeWA: newFakeWA()}
	f.fetchAppState = func(context.Context, string, bool, bool) error {
		fetchCalls.Add(1)
		return errors.New("full sync failed")
	}
	f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
		recoveryCalls.Add(1)
		return "", errors.New("recovery request failed")
	}
	a.wa = f

	var recoveries sync.Map
	name := string(appstate.WAPatchRegularLow)
	recoveries.Store(name, struct{}{})
	a.recoverAppStateCollection(t.Context(), name, &recoveries, time.Second, true)

	if _, loaded := recoveries.Load(name); loaded {
		t.Fatal("in-flight guard remained set after the sequence finished")
	}
	err := fmt.Errorf("failed to verify patch v5848: %w", appstate.ErrMismatchingLTHash)
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name:  appstate.WAPatchRegularLow,
		Error: err,
	}, &recoveries)
	a.appStateRecoveryWorkers.Wait()

	if got := fetchCalls.Load(); got != 1 {
		t.Fatalf("full sync calls = %d, want 1: a failed sequence must defer the next one", got)
	}
	if got := recoveryCalls.Load(); got != 1 {
		t.Fatalf("recovery calls = %d, want 1: a failed sequence must defer the next one", got)
	}
}

func TestAppStateLTHashMismatchAdmitsNextRepairAfterSuccess(t *testing.T) {
	for _, tc := range []struct {
		name           string
		fetchError     error
		recoveryError  error
		wantSnapshots  int32
		wantFetches    int32
		wantRepeat     bool
		wantIntentLeft bool
	}{
		{name: "snapshot-success", wantSnapshots: 1, wantFetches: 0, wantRepeat: true},
		{name: "snapshot-failure-full-success", recoveryError: errors.New("phone offline"), wantSnapshots: 1, wantFetches: 1, wantRepeat: true},
		{name: "both-fail", recoveryError: errors.New("phone offline"), fetchError: errors.New("full sync failed"), wantSnapshots: 1, wantFetches: 1, wantIntentLeft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			var fetches, snapshots atomic.Int32
			f := &appStateContextWA{fakeWA: newFakeWA()}
			f.fetchAppState = func(context.Context, string, bool, bool) error {
				fetches.Add(1)
				return tc.fetchError
			}
			f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
				snapshots.Add(1)
				if tc.recoveryError == nil {
					f.emit(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularLow, Recovery: true})
				}
				return "recovery-req", tc.recoveryError
			}
			a.wa = f
			var recoveries sync.Map
			collection := string(appstate.WAPatchRegularLow)
			recoveries.Store(collection, struct{}{})
			a.recoverAppStateCollection(t.Context(), collection, &recoveries, time.Second, true)
			if _, retained := recoveries.Load(collection); retained {
				t.Fatal("finished sequence left its in-flight guard set")
			}
			if got := fetches.Load(); got != tc.wantFetches {
				t.Fatalf("full sync calls = %d, want %d", got, tc.wantFetches)
			}
			if got := snapshots.Load(); got != tc.wantSnapshots {
				t.Fatalf("snapshot calls = %d, want %d", got, tc.wantSnapshots)
			}
			required, err := a.db.AppStateRecoveryRequired(collection)
			if err != nil || required != tc.wantIntentLeft {
				t.Fatalf("recovery intent = %v, %v; want %v", required, err, tc.wantIntentLeft)
			}

			// The next mismatch is admitted only after a successful repair.
			a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
				Name: appstate.WAPatchRegularLow, Error: appstate.ErrMismatchingLTHash,
			}, &recoveries)
			a.appStateRecoveryWorkers.Wait()
			wantSnapshots := tc.wantSnapshots
			if tc.wantRepeat {
				wantSnapshots *= 2
			}
			if got := snapshots.Load(); got != wantSnapshots {
				t.Fatalf("snapshot calls after second mismatch = %d, want %d", got, wantSnapshots)
			}
		})
	}
}

func TestAppStateNonLTHashErrorDoesNotRequestRecovery(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	var recoveries sync.Map
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name:  appstate.WAPatchRegularLow,
		Error: errors.New("mismatching patch MAC"),
	}, &recoveries)

	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appStateRecoveries) != 0 {
		t.Fatalf("recovery requests = %v, want none", f.appStateRecoveries)
	}
}

func TestAppStateFullRefreshRecordsIntentBeforeCursorAdvance(t *testing.T) {
	a := newTestApp(t)
	f := &appStateContextWA{fakeWA: newFakeWA()}
	called := false
	f.fetchAppState = func(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error {
		called = true
		required, err := a.db.AppStateRecoveryRequired(name)
		if err != nil || !required {
			t.Errorf("recovery intent before full fetch = %v, %v; want true", required, err)
		}
		return nil
	}
	f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
		t.Error("successful full fetch must not request phone recovery")
		return "", nil
	}
	a.wa = f
	var recoveries sync.Map
	recoveries.Store(string(appstate.WAPatchRegularLow), struct{}{})
	a.recoverAppStateCollection(t.Context(), string(appstate.WAPatchRegularLow), &recoveries, time.Second, false)
	if !called {
		t.Fatal("full-fetch callback was not called")
	}
}

func TestFailedAppStateReplayPersistsIntentAndRecoversAtStartup(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Options{StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat := types.JID{User: "15550000001", Server: types.DefaultUserServer}
	if err := a.db.UpsertChat(chat.String(), "dm", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", filepath.Join(dir, "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_recovery_archive BEFORE UPDATE OF archived ON chats BEGIN SELECT RAISE(FAIL, 'injected replay persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	archive := &events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}}
	var snapshots int
	f := &appStateContextWA{fakeWA: newFakeWA()}
	f.fetchEvents = func(context.Context, string, bool, bool) ([]any, error) { return []any{archive}, nil }
	f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
		snapshots++
		return "", errors.New("phone cannot repair local persistence")
	}
	a.wa = f
	var budget sync.Map
	name := string(appstate.WAPatchRegularLow)
	budget.Store(name, struct{}{})
	a.recoverAppStateCollection(t.Context(), name, &budget, time.Second, false)
	if required, err := a.db.AppStateRecoveryRequired(name); err != nil || !required {
		t.Fatalf("failed replay intent = %v, %v; want true", required, err)
	}
	if snapshots != 0 {
		t.Fatalf("local persistence failure requested %d phone snapshots", snapshots)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_recovery_archive`); err != nil {
		t.Fatal(err)
	}
	a.Close()

	for run := range 2 {
		next, err := New(Options{StoreDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		var fullFetches, deltas int
		client := &appStateContextWA{fakeWA: newFakeWA()}
		client.fetchEvents = func(_ context.Context, collection string, full, _ bool) ([]any, error) {
			if collection != name || !full {
				t.Fatalf("recovery fetch = %s, full=%v", collection, full)
			}
			fullFetches++
			if required, err := next.db.AppStateRecoveryRequired(name); err != nil || !required {
				t.Fatalf("startup replay has no intent: %v, %v", required, err)
			}
			return []any{archive}, nil
		}
		client.fetchAppState = func(_ context.Context, collection string, full, _ bool) error {
			if collection == name {
				if full {
					t.Fatal("incremental startup unexpectedly used full fetch")
				}
				deltas++
				client.emit(&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(false)}})
			}
			return nil
		}
		client.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
			t.Error("successful startup replay requested phone recovery")
			return "", nil
		}
		next.wa = client
		if _, err := next.Sync(t.Context(), SyncOptions{Mode: SyncModeOnce, IdleExit: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		stored, err := next.db.GetChat(chat.String())
		if err != nil || stored.Archived != (run == 0) {
			t.Fatalf("run %d chat = %+v, %v", run, stored, err)
		}
		if required, err := next.db.AppStateRecoveryRequired(name); err != nil || required {
			t.Fatalf("run %d recovery intent = %v, %v", run, required, err)
		}
		if run == 0 && (fullFetches != 1 || deltas != 0) || run == 1 && (fullFetches != 0 || deltas != 1) {
			t.Fatalf("run %d full=%d delta=%d", run, fullFetches, deltas)
		}
		next.Close()
	}
}

type recoveryCloseWA struct {
	*appStateContextWA
	disconnected chan struct{}
	closed       atomic.Bool
	once         sync.Once
}

func (f *recoveryCloseWA) Disconnect() {
	f.once.Do(func() { close(f.disconnected) })
	f.fakeWA.Disconnect()
}

func (f *recoveryCloseWA) Close() {
	f.closed.Store(true)
	f.fakeWA.Close()
}

func TestCloseWaitsForAppStateRecoveryPersistence(t *testing.T) {
	a := newTestApp(t)
	chat := types.JID{User: "15550000002", Server: types.DefaultUserServer}
	if err := a.db.UpsertChat(chat.String(), "dm", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer a.Close()
		defer releaseOnce.Do(func() { close(release) })
		f := &recoveryCloseWA{appStateContextWA: &appStateContextWA{fakeWA: newFakeWA()}, disconnected: make(chan struct{})}
		// Fail the snapshot step so the sequence reaches the full replay, whose
		// persistence this test holds open.
		f.requestAppStateRecovery = func(context.Context, string) (types.MessageID, error) {
			return "", errors.New("phone offline")
		}
		f.fetchEvents = func(context.Context, string, bool, bool) ([]any, error) {
			close(started)
			<-release
			return []any{&events.Archive{JID: chat, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)}}}, nil
		}
		a.wa = f
		var budget sync.Map
		a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{Name: appstate.WAPatchRegularLow, Error: appstate.ErrMismatchingLTHash}, &budget)
		<-started
		closed := make(chan struct{})
		go func() { a.Close(); close(closed) }()
		<-f.disconnected
		synctest.Wait()
		if f.closed.Load() {
			t.Fatal("App.Close closed the session store before recovery finished")
		}
		select {
		case <-closed:
			t.Fatal("App.Close closed the database before recovery finished")
		default:
		}
		releaseOnce.Do(func() { close(release) })
		synctest.Wait()
		<-closed
		if !f.closed.Load() {
			t.Fatal("App.Close did not close the session store after recovery finished")
		}
	})
	db, err := store.OpenReadOnly(filepath.Join(a.StoreDir(), "wacli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stored, err := db.GetChat(chat.String())
	if err != nil || !stored.Archived {
		t.Fatalf("recovery was not persisted before close: %+v, %v", stored, err)
	}
}

func TestClosedAppRejectsRecoveryAdmission(t *testing.T) {
	a := newTestApp(t)
	a.wa = newFakeWA()
	a.Close()
	var budget sync.Map
	a.handleAppStateSyncError(t.Context(), &events.AppStateSyncError{
		Name: appstate.WAPatchRegularLow, Error: appstate.ErrMismatchingLTHash,
	}, &budget)
	a.appStateRecoveryWorkers.Wait()
	if _, admitted := budget.Load(string(appstate.WAPatchRegularLow)); admitted {
		t.Fatal("recovery was admitted after application shutdown")
	}
}
