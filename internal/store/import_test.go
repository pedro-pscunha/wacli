package store

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var importAt = time.Date(2026, 5, 4, 10, 0, 0, 0, time.UTC)

// newSourceStore builds an old device's store with fill, closes it, and returns its path.
func newSourceStore(t *testing.T, fill func(old *DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wacli.db")
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fill(old)
	mustDo(t, old.Close())
	return path
}

// counts drops the two paths, so a test compares the numbers only.
func counts(r ImportResult) ImportResult {
	r.Store, r.Source = "", ""
	return r
}

func TestImportFromCopiesOnlyWhatThisStoreLacks(t *testing.T) {
	ctx := context.Background()
	shared, onlyOld, group := "15550000001@s.whatsapp.net", "15550000002@s.whatsapp.net", "120363000000000001@g.us"
	key := []byte("0123456789abcdef0123456789abcdef")
	sha := bytes.Repeat([]byte{1}, 32)
	enc := bytes.Repeat([]byte{2}, 32)
	oldPath := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(shared, "dm", "Old name", importAt))
		mustDo(t, old.UpsertChat(onlyOld, "dm", "Only on the old device", importAt))
		mustDo(t, old.UpsertChat(group, "group", "Obra", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: shared, MsgID: "BOTH", Timestamp: importAt, Text: "old text", Revoked: true}))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: group, MsgID: "PHOTO", SenderJID: "15550000009@s.whatsapp.net",
			Timestamp: importAt, FromMe: true, MediaType: "image", MimeType: "image/jpeg", MediaKey: key, FileSHA256: sha,
			FileEncSHA256: enc, FileLength: 4096}))
		mustDo(t, old.MarkMediaDownloaded(group, "PHOTO", "/Users/someone/old-device/photo.jpg", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: onlyOld, MsgID: "PLACE", Timestamp: importAt, Text: "meet here",
			SenderJID: onlyOld, SenderName: "Ana"}))
		mustDo(t, old.UpsertMessageLocation(MessageLocation{ChatJID: onlyOld, MsgID: "PLACE", Latitude: -23.55, Longitude: -46.63, Name: "Sede"}))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: onlyOld, MsgID: "GONE", Timestamp: importAt, Text: "revoked there", Revoked: true}))
	})

	db := openTestDB(t)
	mustDo(t, db.UpsertChat(shared, "group", "This device's name", importAt))
	mustDo(t, db.UpsertMessage(UpsertMessageParams{ChatJID: shared, MsgID: "BOTH", Timestamp: importAt, Text: "this device's text"}))

	dry, err := db.ImportFrom(ctx, oldPath, ImportOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if counts(dry) != (ImportResult{Chats: 2, Messages: 3, Locations: 1, Existing: 1, DryRun: true}) {
		t.Fatalf("dry run = %+v", dry)
	}
	if dry.Store != db.path || dry.Source != oldPath {
		t.Fatalf("paths = %q, %q", dry.Store, dry.Source)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM messages`); n != 1 {
		t.Fatalf("a dry run wrote: %d messages", n)
	}

	got, err := db.ImportFrom(ctx, oldPath, ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if counts(got) != (ImportResult{Chats: 2, Messages: 3, Locations: 1, Existing: 1}) {
		t.Fatalf("import = %+v", got)
	}

	both, err := db.GetMessage(shared, "BOTH")
	if err != nil || both.Text != "this device's text" || both.Revoked {
		t.Fatalf("a held message changed: %+v, %v", both, err)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM chats WHERE jid = ? AND name = ? AND kind = ?`, shared, "This device's name", "group"); n != 1 {
		t.Fatal("a held chat changed")
	}
	photo, err := db.GetMediaDownloadInfo(group, "PHOTO")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(photo.MediaKey, key) || !bytes.Equal(photo.FileSHA256, sha) || !bytes.Equal(photo.FileEncSHA256, enc) ||
		photo.MediaType != "image" || photo.FileLength != 4096 || photo.DirectPath != "" {
		t.Fatalf("media fields not copied: %+v", photo)
	}
	if photo.LocalPath != "" {
		t.Fatalf("the old device's file path was copied: %q", photo.LocalPath)
	}
	msg, err := db.GetMessage(group, "PHOTO")
	if err != nil || !msg.FromMe || msg.SenderJID != "15550000009@s.whatsapp.net" {
		t.Fatalf("sender or from_me lost: %+v, %v", msg, err)
	}
	gone, err := db.GetMessage(onlyOld, "GONE")
	if err != nil || !gone.Revoked {
		t.Fatalf("a revoked message lost its tombstone: %+v, %v", gone, err)
	}
	place, err := db.GetMessageLocation(onlyOld, "PLACE")
	if err != nil || place.Latitude != -23.55 || place.Name != "Sede" {
		t.Fatalf("place = %+v, %v", place, err)
	}
	if db.ftsEnabled {
		hits, err := db.SearchMessages(SearchMessagesParams{Query: "meet", Limit: 5})
		if err != nil || len(hits) != 1 {
			t.Fatalf("search of a copied message: %d hits, %v", len(hits), err)
		}
	}

	again, err := db.ImportFrom(ctx, oldPath, ImportOptions{})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if counts(again) != (ImportResult{Existing: 4}) {
		t.Fatalf("second import = %+v", again)
	}
}

// The source keeps a hidden-user address that this store resolves to a phone number: the row is compared and copied
// under the phone number, so a message this store holds there is not copied a second time.
func TestImportFromResolvesHiddenUserAddresses(t *testing.T) {
	lid, pn, unknown := "99990000001@lid", "15550000001@s.whatsapp.net", "99990000002@lid"
	oldPath := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(lid, "dm", "", importAt))
		mustDo(t, old.UpsertChat(unknown, "dm", "", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: lid, MsgID: "HELD", SenderJID: lid, Timestamp: importAt, Text: "old copy", Revoked: true}))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: lid, MsgID: "NEW", SenderJID: lid, QuotedMsgID: "HELD", QuotedSenderJID: lid, Timestamp: importAt, Text: "only there"}))
		mustDo(t, old.UpsertMessageLocation(MessageLocation{ChatJID: lid, MsgID: "NEW", Latitude: 1, Longitude: 2}))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: unknown, MsgID: "LID", Timestamp: importAt, Text: "no number yet"}))
	})
	db := openTestDB(t)
	mustDo(t, db.UpsertChat(pn, "dm", "Ana", importAt))
	mustDo(t, db.UpsertMessage(UpsertMessageParams{ChatJID: pn, MsgID: "HELD", SenderJID: pn, Timestamp: importAt, Text: "live copy"}))

	canonical := func(jid string) string { return strings.Replace(jid, lid, pn, 1) }
	got, err := db.ImportFrom(context.Background(), oldPath, ImportOptions{Canonical: canonical})
	if err != nil {
		t.Fatal(err)
	}
	if counts(got) != (ImportResult{Chats: 1, Messages: 2, Locations: 1, Existing: 1, Unresolved: 1}) {
		t.Fatalf("import = %+v", got)
	}
	held, err := db.GetMessage(pn, "HELD")
	if err != nil || held.Text != "live copy" || held.Revoked {
		t.Fatalf("the held message changed: %+v, %v", held, err)
	}
	moved, err := db.GetMessage(pn, "NEW")
	if err != nil || moved.SenderJID != pn || moved.QuotedSenderJID != pn {
		t.Fatalf("copied under the hidden address: %+v, %v", moved, err)
	}
	if _, err := db.GetMessageLocation(pn, "NEW"); err != nil {
		t.Fatalf("place not moved with its message: %v", err)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM chats WHERE jid = ?`, lid); n != 0 {
		t.Fatal("a resolved hidden-user chat was copied")
	}
}

// An @lid chat and its phone-number chat in the same source become one chat here, and the phone-number copy decides.
func TestImportFromCollapsesTwoSourceChatsIntoOne(t *testing.T) {
	lid, pn := "99990000001@lid", "15550000001@s.whatsapp.net"
	oldPath := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(lid, "unknown", "lid name", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: lid, MsgID: "SAME", Timestamp: importAt, Text: "lid copy"}))
		mustDo(t, old.UpsertChat(pn, "dm", "Ana", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: pn, MsgID: "SAME", Timestamp: importAt, Text: "number copy"}))
	})
	db := openTestDB(t)
	got, err := db.ImportFrom(context.Background(), oldPath, ImportOptions{Canonical: func(jid string) string { return strings.Replace(jid, lid, pn, 1) }})
	if err != nil || got.Chats != 1 || got.Messages != 1 || got.Existing != 1 {
		t.Fatalf("import = %+v, %v", got, err)
	}
	msg, err := db.GetMessage(pn, "SAME")
	if err != nil || msg.Text != "number copy" {
		t.Fatalf("the @lid copy won: %+v, %v", msg, err)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM chats WHERE jid = ? AND name = 'Ana' AND kind = 'dm'`, pn); n != 1 {
		t.Fatal("the @lid chat decided the name or kind")
	}
}

// A message this store purged stays purged; it is counted, not reported as copied.
func TestImportFromNeverBringsBackAPurgedMessage(t *testing.T) {
	chat := "15550000003@s.whatsapp.net"
	oldPath := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(chat, "dm", "", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "PURGED", Timestamp: importAt, Text: "secret"}))
	})
	db := openTestDB(t)
	mustDo(t, db.UpsertChat(chat, "dm", "", importAt))
	mustDo(t, db.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "PURGED", Timestamp: importAt, Text: "secret", Revoked: true}))
	mustDo(t, db.PurgeMessage(chat, "PURGED"))
	mustDo(t, db.DeleteChat(chat))
	if n := countRows(t, db.sql, `SELECT count(*) FROM message_payload_purges WHERE chat_jid = ?`, chat); n != 1 {
		t.Fatalf("setup: purge ledger rows = %d", n)
	}
	got, err := db.ImportFrom(context.Background(), oldPath, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages != 0 || got.Purged != 1 || got.Chats != 0 {
		t.Fatalf("import = %+v", got)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM chats WHERE jid = ?`, chat); n != 0 {
		t.Fatal("a chat came back empty")
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM messages WHERE msg_id = 'PURGED'`); n != 0 {
		t.Fatal("a purged message came back")
	}
}

// One bad row fails the whole import and leaves this store as it was; a source without a place table imports.
func TestImportFromIsAllOrNothing(t *testing.T) {
	chat := "15550000004@s.whatsapp.net"
	oldPath := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(chat, "dm", "", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "GOOD", Timestamp: importAt, Text: "fine"}))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "BAD", Timestamp: importAt.Add(time.Second), Text: "x"}))
		if _, err := old.sql.Exec(`UPDATE messages SET buttons = '{not json' WHERE msg_id = 'BAD'`); err != nil {
			t.Fatal(err)
		}
	})
	db := openTestDB(t)
	if _, err := db.ImportFrom(context.Background(), oldPath, ImportOptions{}); err == nil || !strings.Contains(err.Error(), "BAD") {
		t.Fatalf("a bad row did not fail the import by name: %v", err)
	}
	if n := countRows(t, db.sql, `SELECT count(*) FROM messages`) + countRows(t, db.sql, `SELECT count(*) FROM chats`); n != 0 {
		t.Fatalf("a failed import left %d rows", n)
	}

	noPlaces := newSourceStore(t, func(old *DB) {
		mustDo(t, old.UpsertChat(chat, "dm", "", importAt))
		mustDo(t, old.UpsertMessage(UpsertMessageParams{ChatJID: chat, MsgID: "GOOD", Timestamp: importAt, Text: "fine"}))
		if _, err := old.sql.Exec(`DROP TABLE message_locations`); err != nil {
			t.Fatal(err)
		}
	})
	got, err := db.ImportFrom(context.Background(), noPlaces, ImportOptions{})
	if err != nil || got.Messages != 1 || got.Locations != 0 {
		t.Fatalf("a source without places: %+v, %v", got, err)
	}
}

func TestImportFromRefusesThisStore(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ImportFrom(context.Background(), db.path, ImportOptions{DryRun: true}); err == nil {
		t.Fatal("an import from the store itself was accepted")
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
