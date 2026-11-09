package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestImportFromCopiesOnlyWhatThisStoreLacks(t *testing.T) {
	ctx := context.Background()
	shared, onlyOld := "15550000001@s.whatsapp.net", "15550000002@s.whatsapp.net"
	at := time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)

	oldPath := filepath.Join(t.TempDir(), "wacli.db")
	old, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, old.UpsertChat(shared, "dm", "Shared", at))
	mustDo(t, old.UpsertChat(onlyOld, "dm", "Only on the old device", at))
	mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: shared, MsgID: "BOTH", Timestamp: at, Text: "old text"}))
	key := []byte("0123456789abcdef0123456789abcdef")
	mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: shared, MsgID: "PHOTO", Timestamp: at, FromMe: true,
		MediaType: "image", MimeType: "image/jpeg", MediaKey: key, FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc"),
		FileLength: 4096}))
	mustDo(t, old.MarkMediaDownloaded(shared, "PHOTO", "/Users/someone/old-device/photo.jpg", at))
	mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: onlyOld, MsgID: "PLACE", Timestamp: at, Text: "meet here",
		SenderJID: onlyOld, SenderName: "Ana"}))
	mustDo(t, old.UpsertMessageLocation(MessageLocation{ChatJID: onlyOld, MsgID: "PLACE", Latitude: -23.55, Longitude: -46.63, Name: "Sede"}))
	mustDo(t, old.Close())

	db := openTestDB(t)
	mustDo(t, db.UpsertChat(shared, "dm", "Shared", at))
	mustDo(t, db.UpsertMessage(UpsertMessageParams{ChatJID: shared, MsgID: "BOTH", Timestamp: at, Text: "this device's text"}))

	dry, err := db.ImportFrom(ctx, oldPath, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry != (ImportResult{Chats: 1, Messages: 2, Locations: 1, Existing: 1, DryRun: true}) {
		t.Fatalf("dry run = %+v", dry)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM messages`); n != 1 {
		t.Fatalf("a dry run wrote: %d messages", n)
	}

	got, err := db.ImportFrom(ctx, oldPath, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got != (ImportResult{Chats: 1, Messages: 2, Locations: 1, Existing: 1}) {
		t.Fatalf("import = %+v", got)
	}

	both, err := db.GetMessage(shared, "BOTH")
	if err != nil || both.Text != "this device's text" {
		t.Fatalf("a held message changed: %q, %v", both.Text, err)
	}
	photo, err := db.GetMediaDownloadInfo(shared, "PHOTO")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(photo.MediaKey, key) || photo.MediaType != "image" || photo.FileLength != 4096 || photo.DirectPath != "" {
		t.Fatalf("media fields not copied: %+v", photo)
	}
	if photo.LocalPath != "" {
		t.Fatalf("the old device's file path was copied: %q", photo.LocalPath)
	}
	msg, err := db.GetMessage(shared, "PHOTO")
	if err != nil || !msg.FromMe {
		t.Fatalf("from_me lost: %+v, %v", msg, err)
	}
	place, err := db.GetMessageLocation(onlyOld, "PLACE")
	if err != nil || place.Latitude != -23.55 || place.Name != "Sede" {
		t.Fatalf("place = %+v, %v", place, err)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM chats WHERE jid = ? AND name = ?`, onlyOld, "Only on the old device"); n != 1 {
		t.Fatalf("chat of the old device: %d", n)
	}
	if db.ftsEnabled {
		hits, err := db.SearchMessages(SearchMessagesParams{Query: "meet", Limit: 5})
		if err != nil || len(hits) != 1 {
			t.Fatalf("search of a copied message: %d hits, %v", len(hits), err)
		}
	}

	again, err := db.ImportFrom(ctx, oldPath, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again != (ImportResult{Existing: 3}) {
		t.Fatalf("second import = %+v", again)
	}
}

func TestImportFromRefusesThisStore(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportFrom(context.Background(), db.path, true); err == nil {
		t.Fatal("an import from the store itself was accepted")
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
