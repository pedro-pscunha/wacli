package store

import "testing"

func TestPendingSendLifecycle(t *testing.T) {
	d := openTestDB(t)
	p := PendingSend{MsgID: "MSGID0000001", ChatJID: "a@s.whatsapp.net", TypedTo: "+55 11 9", Kind: "text", ContentHash: ContentHash("text", "hi")}
	got, inserted, err := d.BeginPendingSend(p)
	if err != nil || !inserted || got.State != PendingSendInFlight {
		t.Fatalf("first begin: %+v %v %v", got, inserted, err)
	}
	// A retry finds the unfinished attempt, with the typed recipient and hash.
	got, inserted, err = d.BeginPendingSend(p)
	if err != nil || inserted || got.State != PendingSendInFlight || got.TypedTo != "+55 11 9" || got.ContentHash != p.ContentHash {
		t.Fatalf("second begin: %+v %v %v", got, inserted, err)
	}
	if err := d.MarkPendingSendSent("MSGID0000001", "b@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	got, inserted, _ = d.BeginPendingSend(p)
	if inserted || got.State != PendingSendSent || got.ChatJID != "b@s.whatsapp.net" {
		t.Fatalf("after mark: %+v %v", got, inserted)
	}
	if list, _ := d.ListPendingSends(); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	if ok, err := d.FinishPendingSend("MSGID0000001"); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if ok, _ := d.FinishPendingSend("MSGID0000001"); ok {
		t.Fatal("finishing a missing row reported a removal")
	}
	if _, inserted, _ = d.BeginPendingSend(p); !inserted {
		t.Fatal("a finished send must not block a new attempt")
	}
	if _, _, err := d.BeginPendingSend(PendingSend{MsgID: " "}); err == nil {
		t.Fatal("an empty id was accepted")
	}
}

func TestPendingSendExpires(t *testing.T) {
	d := openTestDB(t)
	p := PendingSend{MsgID: "OLDMSGID0001", ChatJID: "a@s.whatsapp.net"}
	if _, _, err := d.BeginPendingSend(p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE pending_sends SET created_at = created_at - ?`, int64(pendingSendRetention.Seconds())+60); err != nil {
		t.Fatal(err)
	}
	if _, inserted, _ := d.BeginPendingSend(p); !inserted {
		t.Fatal("an expired pending send still blocks")
	}
}

func TestContentHashDistinguishesKindAndText(t *testing.T) {
	if ContentHash("text", "a") == ContentHash("text", "b") || ContentHash("text", "a") == ContentHash("poll", "a") {
		t.Fatal("hash collision across kind or text")
	}
	if ContentHash("text", " a ") != ContentHash("text", "a") {
		t.Fatal("surrounding space must not change the hash")
	}
}

// m-4: this fork's migrations live in a reserved range. A store that applied the
// pre-release 33 and 34 is renumbered, not migrated twice, and an upstream row
// with the same number but another name is left alone.
func TestForkMigrationsAreRenumbered(t *testing.T) {
	d := openTestDB(t)
	for _, v := range []int{forkMigrationBase + 1, forkMigrationBase + 2} {
		var n int
		if err := d.sql.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, v).Scan(&n); err != nil || n != 1 {
			t.Fatalf("migration %d recorded %d times (%v)", v, n, err)
		}
	}
	var low int
	_ = d.sql.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version IN (33, 34)`).Scan(&low)
	if low != 0 {
		t.Fatalf("%d rows still use upstream's 33/34 numbers", low)
	}

	// A store from the earlier build of this branch: rows 33 and 34 by our names.
	if _, err := d.sql.Exec(`DELETE FROM schema_migrations WHERE version > 1000`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(33,'own message id lookup index',1),(34,'pending sends',1)`); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, _ := d.sql.Query(`SELECT name FROM schema_migrations WHERE version > 1000 ORDER BY version`)
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if len(names) != 2 || names[0] != "fork: own message id lookup index" || names[1] != "fork: pending sends" {
		t.Fatalf("renumbered rows = %v", names)
	}

	// An upstream migration 33 with another name must not be touched.
	if _, err := d.sql.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(33,'upstream thing',1)`); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := d.sql.QueryRow(`SELECT name FROM schema_migrations WHERE version = 33`).Scan(&name); err != nil || name != "upstream thing" {
		t.Fatalf("upstream row changed: %q %v", name, err)
	}
}

// ensureCurrentSchema repairs a store whose pending_sends came from the first
// version of the table, before kind, content_hash and typed_to existed.
func TestPendingSendsOldTableGetsNewColumns(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.sql.Exec(`DROP TABLE pending_sends; CREATE TABLE pending_sends (msg_id TEXT PRIMARY KEY, chat_jid TEXT NOT NULL, state TEXT NOT NULL, created_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	p := PendingSend{MsgID: "COLSMSGID001", ChatJID: "a@s.whatsapp.net", TypedTo: "x", Kind: "text", ContentHash: "h"}
	if _, inserted, err := d.BeginPendingSend(p); err != nil || !inserted {
		t.Fatalf("begin on the repaired table: %v %v", inserted, err)
	}
}
