package store

import "testing"

func TestPendingSendLifecycle(t *testing.T) {
	d := openTestDB(t)
	state, inserted, err := d.BeginPendingSend("MSGID0000001", "a@s.whatsapp.net")
	if err != nil || !inserted || state != PendingSendInFlight {
		t.Fatalf("first begin: %q %v %v", state, inserted, err)
	}
	// A retry finds the unfinished attempt.
	state, inserted, err = d.BeginPendingSend("MSGID0000001", "a@s.whatsapp.net")
	if err != nil || inserted || state != PendingSendInFlight {
		t.Fatalf("second begin: %q %v %v", state, inserted, err)
	}
	if err := d.MarkPendingSendSent("MSGID0000001"); err != nil {
		t.Fatal(err)
	}
	if state, inserted, _ = d.BeginPendingSend("MSGID0000001", "a@s.whatsapp.net"); inserted || state != PendingSendSent {
		t.Fatalf("after mark: %q %v", state, inserted)
	}
	if err := d.FinishPendingSend("MSGID0000001"); err != nil {
		t.Fatal(err)
	}
	if _, inserted, _ = d.BeginPendingSend("MSGID0000001", "a@s.whatsapp.net"); !inserted {
		t.Fatal("a finished send must not block a new attempt")
	}
	if _, _, err := d.BeginPendingSend(" ", "x"); err == nil {
		t.Fatal("an empty id was accepted")
	}
}

func TestPendingSendExpires(t *testing.T) {
	d := openTestDB(t)
	if _, _, err := d.BeginPendingSend("OLDMSGID0001", "a@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE pending_sends SET created_at = created_at - ?`, int64(pendingSendRetention.Seconds())+60); err != nil {
		t.Fatal(err)
	}
	if _, inserted, _ := d.BeginPendingSend("OLDMSGID0001", "a@s.whatsapp.net"); !inserted {
		t.Fatal("an expired pending send still blocks")
	}
}
