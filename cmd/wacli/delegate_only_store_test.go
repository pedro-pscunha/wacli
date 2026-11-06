package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
)

// M3: a refused call touches neither the store lock nor wacli.db.
func TestDelegateOnlyRefusalTakesNoLockAndOpensNoDatabase(t *testing.T) {
	store := t.TempDir()
	err := execute([]string{"--store", store, "--delegate-only", "send", "text", "--to", "5511999999999", "--message", "x"})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"wacli.db", "session.db", "LOCK"} {
		if _, statErr := os.Stat(filepath.Join(store, name)); statErr == nil {
			t.Errorf("%s was created by a refused call", name)
		}
	}
	// And the lock stays free for the daemon.
	lk, lockErr := lock.Acquire(store)
	if lockErr != nil {
		t.Fatalf("the store lock is held after a refusal: %v", lockErr)
	}
	_ = lk.Release()
}

// M2a: a per-store marker pins the store with no flag and no environment.
func TestMarkerPinsStoreToDaemon(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := execute([]string{"--store", store, "send", "text", "--to", "5511999999999", "--message", "x"})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("marker not honored: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(store, "wacli.db")); statErr == nil {
		t.Error("wacli.db was created")
	}
}

func TestAccountConfigPinsStoreToDaemon(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	cfgPath := config.DefaultConfigPath()
	store := filepath.Join(t.TempDir(), "work1")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.AccountsConfig{Accounts: map[string]config.AccountEntry{
		"work1": {Store: store, DelegateOnly: true},
		"free":  {Store: filepath.Join(t.TempDir(), "free")},
	}}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if !config.StoreIsDelegateOnly(store, cfgPath) {
		t.Fatal("delegate_only account not honored")
	}
	if config.StoreIsDelegateOnly(filepath.Join(t.TempDir(), "other"), cfgPath) {
		t.Fatal("an unrelated store was pinned")
	}
	err := execute([]string{"--account", "work1", "send", "text", "--to", "5511999999999", "--message", "x"})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("err = %v", err)
	}
	// Roundtrip: the key survives a save and a load.
	got, _, _ := config.LoadAccountsConfigIfExists(cfgPath)
	if !got.Accounts["work1"].DelegateOnly || got.Accounts["free"].DelegateOnly {
		t.Fatalf("config roundtrip: %+v", got.Accounts)
	}
}

// With the daemon's socket up, a delegate-only call delegates and never opens the store.
func TestDelegateOnlyStillDelegatesWhenDaemonListens(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Sent: true, To: "x@s.whatsapp.net", ID: "IDFROMDAEMON1"}
	})
	defer server.stop()
	// the socket accepts one connection; the availability probe must not use it up
	stdout, stderr, runErr := runPresenceDelegateHelper(t, []string{"--store", storeDir, "--delegate-only", "--json", "--timeout", "750ms", "send", "text", "--to", "+15551234567", "--message", "hi"})
	if runErr != nil {
		t.Fatalf("delegate-only with a daemon failed: %v stdout=%q stderr=%q", runErr, stdout, stderr)
	}
	if req := server.nextRequest(t); req.Kind != "text" {
		t.Fatalf("req = %+v", req)
	}
}

func TestDevicesNeverConnectsOutsideDaemon(t *testing.T) {
	store := t.TempDir()
	err := execute([]string{"--store", store, "devices"})
	if err == nil || !strings.Contains(err.Error(), "daemon not running") {
		t.Fatalf("err = %v", err)
	}
	for _, name := range []string{"wacli.db", "session.db", "LOCK"} {
		if _, statErr := os.Stat(filepath.Join(store, name)); statErr == nil {
			t.Errorf("devices created %s", name)
		}
	}
}

// m10: --delegate-only (or the environment) must not stop the daemon itself.
// sync --follow takes the lock and starts; it fails later, on the missing
// session, never with the delegate-only refusal.
func TestSyncFollowIsNotRefusedUnderDelegateOnlyEnv(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "1")
	store := t.TempDir()
	err := execute([]string{"--store", store, "sync", "--follow"})
	if err != nil && strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("the daemon was refused: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("err = %v, want the unauthenticated-store error that follows a normal start", err)
	}
}

func TestSyncFollowIsNotRefusedUnderMarker(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := execute([]string{"--store", store, "sync", "--follow"})
	if err != nil && strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("the daemon was refused by its own marker: %v", err)
	}
}

// An authenticated store with no daemon: the refusal must come from the gate,
// before anything opens the store, and name the cause, not "not authenticated".
func TestDelegateOnlyRefusesOnAuthenticatedStoreWithoutDaemon(t *testing.T) {
	f := newDaemonFakeWA() // IsAuthed() is true
	store := t.TempDir()
	a := newDaemonAppAt(t, f, store)
	a.Close() // leave a real store on disk, as a daemon that ran before would
	err := execute([]string{"--store", store, "--delegate-only", "messages", "forward", "--chat", "5511999999999", "--id", "ABCDEFGH12", "--to", "5511888888888"})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteForMeRefusesNewID(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "messages", "delete", "--chat", "c", "--id", "ABCDEFGH12", "--for-me", "--new-id", "3EB0ABCDEF0123456789"})
	if err == nil || !strings.Contains(err.Error(), "--new-id does not apply to --for-me") {
		t.Fatalf("err = %v", err)
	}
}

// m6: the lock is held but no socket answers: the documented wording, so callers
// that match on it classify the refusal correctly.
func TestLockedStoreWithoutSocketUsesDocumentedWordingWhenDelegateOnly(t *testing.T) {
	store := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(store)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	_, stderr, runErr := runPresenceDelegateHelper(t, []string{"--store", store, "--delegate-only", "--timeout", "750ms", "send", "text", "--to", "+15551234567", "--message", "x"})
	if runErr == nil || !strings.Contains(stderr, "refusing to connect outside the sync daemon") {
		t.Fatalf("err=%v stderr=%q", runErr, stderr)
	}
}

func TestCapabilitiesDoNotAdvertiseClientFlags(t *testing.T) {
	for _, f := range currentCapabilities().Features {
		if f == "delegate_only" {
			t.Fatal("delegate_only is a client flag, not a daemon feature")
		}
	}
}
