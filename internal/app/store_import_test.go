package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// The command's import resolves a hidden-user address with this account's LID map, as sync stores it.
func TestImportStoreUsesThisAccountsLIDMap(t *testing.T) {
	lid := types.NewJID("99990000001", types.HiddenUserServer)
	pn := types.NewJID("15550000001", types.DefaultUserServer)
	sourcePath := filepath.Join(t.TempDir(), "wacli.db")
	source, err := store.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)
	if err := source.UpsertChat(lid.String(), "dm", "", at); err != nil {
		t.Fatal(err)
	}
	if err := source.UpsertMessage(store.UpsertMessageParams{ChatJID: lid.String(), MsgID: "OLD", SenderJID: lid.String(), Timestamp: at, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	f := newFakeWA()
	f.lids[lid] = pn
	a.wa = f
	got, err := a.ImportStore(context.Background(), sourcePath, false)
	if err != nil || got.Messages != 1 || got.Unresolved != 0 {
		t.Fatalf("import = %+v, %v", got, err)
	}
	msg, err := a.db.GetMessage(pn.String(), "OLD")
	if err != nil || msg.SenderJID != pn.String() {
		t.Fatalf("not stored under the phone number: %+v, %v", msg, err)
	}
}
