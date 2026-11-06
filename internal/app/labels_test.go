package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestEditLabelSendsOneMutation(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	if err := a.EditLabel(context.Background(), "19", "Bancos", 13, false); err != nil {
		t.Fatalf("EditLabel: %v", err)
	}

	if len(f.labelEditCalls) != 1 {
		t.Fatalf("expected 1 label edit, got %d", len(f.labelEditCalls))
	}
	got := f.labelEditCalls[0]
	if got.labelID != "19" || got.name != "Bancos" || got.color != 13 || got.deleted {
		t.Fatalf("unexpected label edit: %+v", got)
	}
}

func TestEditLabelDeletePassesDeletedFlag(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f

	if err := a.EditLabel(context.Background(), "25", "Cobranca", 0, true); err != nil {
		t.Fatalf("EditLabel: %v", err)
	}
	if !f.labelEditCalls[0].deleted {
		t.Fatal("delete must set the deleted flag")
	}
}

func TestLabelChatAttachAndDetach(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	chat := types.NewJID("5511999999999", types.DefaultUserServer)

	if err := a.LabelChat(context.Background(), chat, "19", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := a.LabelChat(context.Background(), chat, "19", false); err != nil {
		t.Fatalf("detach: %v", err)
	}

	if len(f.labelChatCalls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(f.labelChatCalls))
	}
	if !f.labelChatCalls[0].labeled || f.labelChatCalls[1].labeled {
		t.Fatalf("attach then detach not recorded: %+v", f.labelChatCalls)
	}
	if f.labelChatCalls[0].target != chat {
		t.Fatalf("wrong target: %v", f.labelChatCalls[0].target)
	}
}

func TestLabelWriteSurfacesServerError(t *testing.T) {
	// A 409 conflict from WhatsApp must reach the caller, never be swallowed:
	// a silent failure would let a retry loop create duplicate labels.
	a := newTestApp(t)
	f := newFakeWA()
	sentinel := errors.New("server returned error updating app state (regular): conflict")
	f.labelErr = sentinel
	a.wa = f

	if err := a.EditLabel(context.Background(), "1", "X", 0, false); !errors.Is(err, sentinel) {
		t.Fatalf("expected the server error, got %v", err)
	}
	chat := types.NewJID("5511999999999", types.DefaultUserServer)
	if err := a.LabelChat(context.Background(), chat, "1", true); !errors.Is(err, sentinel) {
		t.Fatalf("expected the server error, got %v", err)
	}
}

func TestLabelWriteRepairsLaggingVersionBeforeSend(t *testing.T) {
	// Every patch the phone writes to a regular chain that fails verification
	// leaves the local version one behind head, and a send from there is a
	// guaranteed 409. The pre-write sync must see the mismatch, restore the head
	// from a primary-device snapshot, and only then send.
	a := newTestApp(t)
	f := newFakeWA()
	f.appStateFetchErrs = []error{fmt.Errorf("failed to verify patch v1396: %w", appstate.ErrMismatchingLTHash)}
	var sendsAtRecovery int
	f.onAppStateRecovery = func(name string) {
		f.mu.Lock()
		sendsAtRecovery = len(f.labelChatCalls)
		f.mu.Unlock()
		f.emit(&events.AppStateSyncComplete{Name: appstate.WAPatchName(name), Recovery: true})
	}
	a.wa = f
	chat := types.NewJID("5511999999999", types.DefaultUserServer)

	if err := a.LabelChat(context.Background(), chat, "7", true); err != nil {
		t.Fatalf("LabelChat: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appStateFetches) != 1 || f.appStateFetches[0].name != string(appstate.WAPatchRegular) || f.appStateFetches[0].fullSync {
		t.Fatalf("pre-write fetches = %+v, want one incremental regular fetch and no full replay", f.appStateFetches)
	}
	if len(f.appStateRecoveries) != 1 || f.appStateRecoveries[0] != string(appstate.WAPatchRegular) {
		t.Fatalf("recoveries = %v, want one regular snapshot", f.appStateRecoveries)
	}
	if sendsAtRecovery != 0 || len(f.labelChatCalls) != 1 {
		t.Fatalf("send order: %d sends before the snapshot, %d total; want 0 then 1", sendsAtRecovery, len(f.labelChatCalls))
	}
	if required, err := a.db.AppStateRecoveryRequired(string(appstate.WAPatchRegular)); err != nil || required {
		t.Fatalf("recovery intent after a clean label write = %v, %v; want none", required, err)
	}
}

func TestLabelWriteDoesNotSendWhenRepairFails(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	mismatch := fmt.Errorf("failed to verify patch v1396: %w", appstate.ErrMismatchingLTHash)
	f.appStateFetchErrs = []error{mismatch, mismatch}
	f.appStateRecoveryErr = errors.New("phone offline")
	a.wa = f

	err := a.LabelChat(context.Background(), types.NewJID("5511999999999", types.DefaultUserServer), "7", true)
	if err == nil {
		t.Fatal("LabelChat succeeded although the collection could not be repaired")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.labelChatCalls) != 0 {
		t.Fatalf("label sent from a lagging version: %+v", f.labelChatCalls)
	}
	if len(f.appStateFetches) != 2 || !f.appStateFetches[1].fullSync {
		t.Fatalf("fetches = %+v, want incremental then full replay fallback", f.appStateFetches)
	}
}

func TestReadLabelMutationsUsesRegularCollection(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.appStateSnapshot = []appstate.Mutation{{Index: []string{appstate.IndexLabelEdit, "3"}}}
	a.wa = f

	got, err := a.ReadLabelMutations(context.Background())
	if err != nil {
		t.Fatalf("ReadLabelMutations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the snapshot through, got %d mutations", len(got))
	}
	if len(f.appStateSnapshotFetches) != 1 || f.appStateSnapshotFetches[0] != string(appstate.WAPatchRegular) {
		t.Fatalf("labels must read the regular collection, got %v", f.appStateSnapshotFetches)
	}
}

func TestReadLabelMutationsPropagatesError(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	sentinel := errors.New("mismatching LTHash")
	f.appStateSnapshotErr = sentinel
	a.wa = f

	if _, err := a.ReadLabelMutations(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("expected the fetch error, got %v", err)
	}
}

func TestLabelChatTargetsByIdentity(t *testing.T) {
	// WhatsApp keys a label membership on the contact's LID. An attach must go
	// to the LID when the session knows one, and a detach must clear both forms
	// so an entry written earlier on the phone JID stops surviving a removal
	// made on the phone.
	const (
		pn  = "5511999990001@s.whatsapp.net"
		lid = "111222333444555@lid"
	)
	known := types.NewJID("5511999990001", types.DefaultUserServer)
	unknown := types.NewJID("5511900000000", types.DefaultUserServer)

	cases := []struct {
		name    string
		target  types.JID
		labeled bool
		want    []string
	}{
		{"attach on a phone JID with a known LID", known, true, []string{lid}},
		{"attach on a phone JID without a LID", unknown, true, []string{"5511900000000@s.whatsapp.net"}},
		{"attach on a LID", types.NewJID("111222333444555", types.HiddenUserServer), true, []string{lid}},
		{"attach on a group", types.NewJID("120363000000000001", types.GroupServer), true, []string{"120363000000000001@g.us"}},
		{"detach clears both forms", known, false, []string{lid, pn}},
		{"detach without a LID clears the phone form", unknown, false, []string{"5511900000000@s.whatsapp.net"}},
		{"detach on a group", types.NewJID("120363000000000001", types.GroupServer), false, []string{"120363000000000001@g.us"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			f.lids[types.NewJID("111222333444555", types.HiddenUserServer)] = types.NewJID("5511999990001", types.DefaultUserServer)
			a.wa = f

			got := a.labelChatTargets(context.Background(), tc.target, "7", tc.labeled)
			if len(got) != len(tc.want) {
				t.Fatalf("targets = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i].String() != tc.want[i] {
					t.Fatalf("target %d = %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLabelChatDetachSendsBothForms(t *testing.T) {
	a := newTestApp(t)
	f := newFakeWA()
	f.lids[types.NewJID("111222333444555", types.HiddenUserServer)] = types.NewJID("5511999990001", types.DefaultUserServer)
	a.wa = f
	chat := types.NewJID("5511999990001", types.DefaultUserServer)

	if err := a.LabelChat(context.Background(), chat, "7", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := a.LabelChat(context.Background(), chat, "7", false); err != nil {
		t.Fatalf("detach: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.labelChatCalls) != 3 {
		t.Fatalf("expected 1 attach and 2 detach mutations, got %+v", f.labelChatCalls)
	}
	if got := f.labelChatCalls[0].target.String(); got != "111222333444555@lid" {
		t.Fatalf("attach went to %s, want the LID", got)
	}
	if got := f.labelChatCalls[1].target.String(); got != "111222333444555@lid" {
		t.Fatalf("first detach went to %s, want the LID", got)
	}
	if got := f.labelChatCalls[2].target.String(); got != "5511999990001@s.whatsapp.net" {
		t.Fatalf("second detach went to %s, want the phone JID", got)
	}
	for _, c := range f.labelChatCalls[1:] {
		if c.labeled {
			t.Fatalf("detach recorded as an attach: %+v", c)
		}
	}
}

func TestLabelChatStopsOnTheFirstFailedForm(t *testing.T) {
	// A failed delete must reach the caller instead of leaving the caller to
	// believe both forms are gone.
	a := newTestApp(t)
	f := newFakeWA()
	f.lids[types.NewJID("111222333444555", types.HiddenUserServer)] = types.NewJID("5511999990001", types.DefaultUserServer)
	sentinel := errors.New("server returned error updating app state (regular): conflict")
	f.labelErr = sentinel
	a.wa = f

	err := a.LabelChat(context.Background(), types.NewJID("5511999990001", types.DefaultUserServer), "7", false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the server error, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.labelChatCalls) != 1 {
		t.Fatalf("the second form must not be sent after a failure: %+v", f.labelChatCalls)
	}
}
