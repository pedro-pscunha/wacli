package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/config"
)

// m-5: `auth --follow` keeps syncing with no delegate server, so on a pinned
// store it is refused; plain `auth` stays allowed.
func TestAuthFollowIsRefusedOnAPinnedStore(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	store := t.TempDir()
	if err := os.WriteFile(filepath.Join(store, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := execute([]string{"--store", store, "auth", "--follow", "--phone", "5511999999999"})
	if err == nil || !strings.Contains(err.Error(), "auth --follow is refused on a store pinned to the sync daemon") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "stop the unit") {
		t.Fatalf("the error must say to stop the unit first: %v", err)
	}
	// Without --follow the pin does not stop pairing.
	err = execute([]string{"--store", store, "auth", "--phone", "not-a-number"})
	if err != nil && strings.Contains(err.Error(), "pinned to the sync daemon") {
		t.Fatalf("plain auth was refused: %v", err)
	}
	// An unpinned store may still use --follow.
	free := t.TempDir()
	err = execute([]string{"--store", free, "auth", "--follow", "--phone", "not-a-number"})
	if err != nil && strings.Contains(err.Error(), "pinned to the sync daemon") {
		t.Fatalf("an unpinned store was refused: %v", err)
	}
}
