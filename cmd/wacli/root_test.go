package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/config"
)

func captureRootStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}

func captureRootStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}

func TestWriteRootErrorEventsUsesNDJSON(t *testing.T) {
	raw := captureRootStderr(t, func() {
		writeRootError(rootFlags{events: true}, errors.New("boom"))
	})

	var evt struct {
		Event string         `json:"event"`
		Data  map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("root error was not NDJSON: %q: %v", raw, err)
	}
	if evt.Event != "error" {
		t.Fatalf("event = %q, want error", evt.Event)
	}
	if evt.Data["message"] != "boom" {
		t.Fatalf("message = %#v, want boom", evt.Data["message"])
	}
}

func TestRootFlagsReadOnlyFlag(t *testing.T) {
	flags := &rootFlags{readOnly: true}

	if !flags.isReadOnly() {
		t.Fatal("isReadOnly = false, want true")
	}
	err := flags.requireWritable()
	if err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Fatalf("requireWritable error = %v", err)
	}
}

func TestRootFlagsReadOnlyEnv(t *testing.T) {
	t.Setenv("WACLI_READONLY", "yes")

	if !(&rootFlags{}).isReadOnly() {
		t.Fatal("isReadOnly = false, want true")
	}
}

func TestResolveStoreDirAccount(t *testing.T) {
	isolateAccountConfigHome(t)
	cfgPath := config.DefaultConfigPath()
	cfg := &config.AccountsConfig{
		Accounts: map[string]config.AccountEntry{
			"work": {Store: "accounts/work"},
		},
	}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := resolveStoreDir(&rootFlags{account: "work"})
	if err != nil {
		t.Fatalf("resolveStoreDir: %v", err)
	}
	want := filepath.Join(filepath.Dir(cfgPath), "accounts", "work")
	if got != want {
		t.Fatalf("storeDir = %q, want %q", got, want)
	}
}

func TestResolveStoreDirStoreAndAccountConflict(t *testing.T) {
	_, err := resolveStoreDir(&rootFlags{storeDir: "/tmp/wacli", account: "work"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("resolveStoreDir error = %v, want conflict", err)
	}
}

func TestResolveStoreDirEnvBeatsDefaultAccount(t *testing.T) {
	isolateAccountConfigHome(t)
	cfgPath := config.DefaultConfigPath()
	cfg := &config.AccountsConfig{
		DefaultAccount: "work",
		Accounts: map[string]config.AccountEntry{
			"work": {Store: "accounts/work"},
		},
	}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	envStore := filepath.Join(t.TempDir(), "env-store")
	t.Setenv(config.EnvStoreDir, envStore)

	got, err := resolveStoreDir(&rootFlags{})
	if err != nil {
		t.Fatalf("resolveStoreDir: %v", err)
	}
	if got != envStore {
		t.Fatalf("storeDir = %q, want %q", got, envStore)
	}
}

func TestResolveStoreDirAccountsWithoutDefaultRequireAccount(t *testing.T) {
	isolateAccountConfigHome(t)
	cfgPath := config.DefaultConfigPath()
	cfg := &config.AccountsConfig{
		Accounts: map[string]config.AccountEntry{
			"work":     {Store: "accounts/work"},
			"personal": {Store: "accounts/personal"},
		},
	}
	if err := config.SaveAccountsConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	_, err := resolveStoreDir(&rootFlags{})
	if err == nil || !strings.Contains(err.Error(), "pass --account (one of: personal, work)") {
		t.Fatalf("resolveStoreDir error = %v, want a refusal that names the accounts", err)
	}
	got, err := resolveStoreDir(&rootFlags{account: "personal"})
	if err != nil {
		t.Fatalf("resolveStoreDir --account: %v", err)
	}
	if want := filepath.Join(filepath.Dir(cfgPath), "accounts", "personal"); got != want {
		t.Fatalf("storeDir = %q, want %q", got, want)
	}
	envStore := filepath.Join(t.TempDir(), "env-store")
	t.Setenv(config.EnvStoreDir, envStore)
	if got, err := resolveStoreDir(&rootFlags{}); err != nil || got != envStore {
		t.Fatalf("storeDir = %q, %v; want the WACLI_STORE_DIR store", got, err)
	}
}

func TestResolveStoreDirNoConfigUsesBaseStore(t *testing.T) {
	isolateAccountConfigHome(t)
	got, err := resolveStoreDir(&rootFlags{})
	if err != nil {
		t.Fatalf("resolveStoreDir: %v", err)
	}
	if want, _ := filepath.Abs(config.DefaultStoreDir()); got != want {
		t.Fatalf("storeDir = %q, want %q", got, want)
	}
}

func isolateAccountConfigHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv(config.EnvStoreDir, "")
}
