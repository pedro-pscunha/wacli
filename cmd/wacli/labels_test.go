package main

import (
	"testing"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func labelEditMutation(id, name string, color int32, deleted bool) appstate.Mutation {
	return appstate.Mutation{
		Index: []string{appstate.IndexLabelEdit, id},
		Action: &waSyncAction.SyncActionValue{
			LabelEditAction: &waSyncAction.LabelEditAction{
				Name:    proto.String(name),
				Color:   proto.Int32(color),
				Deleted: proto.Bool(deleted),
			},
		},
	}
}

func labelChatMutation(id, jid string, labeled bool) appstate.Mutation {
	return appstate.Mutation{
		Index: []string{appstate.IndexLabelAssociationChat, id, jid},
		Action: &waSyncAction.SyncActionValue{
			LabelAssociationAction: &waSyncAction.LabelAssociationAction{
				Labeled: proto.Bool(labeled),
			},
		},
	}
}

func TestCollectLabelsFoldsEditsAndAssociations(t *testing.T) {
	rows := collectLabels([]appstate.Mutation{
		labelEditMutation("3", "Pending payment", 0, false),
		labelEditMutation("19", "Banco", 13, false),
		labelChatMutation("19", "555@s.whatsapp.net", true),
		labelChatMutation("19", "111@s.whatsapp.net", true),
		labelChatMutation("3", "222@s.whatsapp.net", true),
	})

	if len(rows) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(rows))
	}
	// Rows sort by name, so Banco precedes Pending payment.
	if rows[0].Name != "Banco" || rows[0].ID != "19" || rows[0].Color != 13 {
		t.Fatalf("unexpected first label: %+v", rows[0])
	}
	if len(rows[0].Chats) != 2 {
		t.Fatalf("expected 2 chats on Banco, got %d", len(rows[0].Chats))
	}
	// Chats sort by JID.
	if rows[0].Chats[0].JID != "111@s.whatsapp.net" {
		t.Fatalf("chats are not sorted: %+v", rows[0].Chats)
	}
	if rows[1].Name != "Pending payment" || len(rows[1].Chats) != 1 {
		t.Fatalf("unexpected second label: %+v", rows[1])
	}
}

func TestCollectLabelsDropsUnlabeledChats(t *testing.T) {
	rows := collectLabels([]appstate.Mutation{
		labelEditMutation("7", "Urgente", 6, false),
		labelChatMutation("7", "555@s.whatsapp.net", true),
		labelChatMutation("7", "555@s.whatsapp.net", false),
	})

	if len(rows) != 1 {
		t.Fatalf("expected 1 label, got %d", len(rows))
	}
	if len(rows[0].Chats) != 0 {
		t.Fatalf("a detached chat must not be listed: %+v", rows[0].Chats)
	}
}

func TestCollectLabelsKeepsLabelWithoutEditMutation(t *testing.T) {
	// A snapshot can carry an association whose label_edit entry is absent.
	rows := collectLabels([]appstate.Mutation{
		labelChatMutation("42", "555@s.whatsapp.net", true),
	})

	if len(rows) != 1 {
		t.Fatalf("expected 1 label, got %d", len(rows))
	}
	if rows[0].ID != "42" || rows[0].Name != "" {
		t.Fatalf("unexpected label: %+v", rows[0])
	}
	if len(rows[0].Chats) != 1 {
		t.Fatalf("expected the association to survive: %+v", rows[0].Chats)
	}
}

func TestCollectLabelsIgnoresMalformedMutations(t *testing.T) {
	rows := collectLabels([]appstate.Mutation{
		{Index: nil, Action: nil},
		{Index: []string{appstate.IndexLabelEdit}, Action: &waSyncAction.SyncActionValue{}},
		{Index: []string{appstate.IndexLabelAssociationChat, "9"}, Action: &waSyncAction.SyncActionValue{}},
		{Index: []string{"mute", "555@s.whatsapp.net"}, Action: &waSyncAction.SyncActionValue{}},
	})

	if len(rows) != 0 {
		t.Fatalf("expected no labels, got %+v", rows)
	}
}

func TestMergeLabelChatsJoinsBothFormsOfOneChat(t *testing.T) {
	// WhatsApp keys a membership on the LID. A phone-form entry written by an
	// older client is a leftover, and the list must show one person, while it
	// keeps both raw forms so a repair can still find the leftover.
	toPhone := func(lid types.JID) string {
		if lid.User == "111222333444555" {
			return "5511999990001@s.whatsapp.net"
		}
		return lid.String()
	}
	name := func(raw, phone string) string {
		if phone == "5511999990001@s.whatsapp.net" {
			return "Test Contact"
		}
		return ""
	}

	got := mergeLabelChats([]labelChat{
		{JID: "111222333444555@lid"},
		{JID: "5511999990001@s.whatsapp.net"},
		{JID: "120363000000000001@g.us"},
	}, toPhone, name)

	if len(got) != 2 {
		t.Fatalf("expected 2 rows after the merge, got %d: %+v", len(got), got)
	}
	person := got[0]
	if person.JID != "111222333444555@lid" {
		t.Fatalf("the LID form must name the row, got %q", person.JID)
	}
	if person.PhoneJID != "5511999990001@s.whatsapp.net" {
		t.Fatalf("phone JID = %q", person.PhoneJID)
	}
	if person.Name != "Test Contact" {
		t.Fatalf("name = %q", person.Name)
	}
	if len(person.JIDs) != 2 || person.JIDs[0] != "111222333444555@lid" || person.JIDs[1] != "5511999990001@s.whatsapp.net" {
		t.Fatalf("both raw forms must survive the merge: %+v", person.JIDs)
	}
	if got[1].JID != "120363000000000001@g.us" || got[1].PhoneJID != "" {
		t.Fatalf("a group must pass through unchanged: %+v", got[1])
	}
}

func TestMergeLabelChatsKeepsUnresolvedLIDApart(t *testing.T) {
	// With no mapping the LID keys under itself, so two unknown people never
	// collapse into one row.
	toPhone := func(lid types.JID) string { return lid.String() }
	got := mergeLabelChats([]labelChat{
		{JID: "111@lid"},
		{JID: "222@lid"},
	}, toPhone, func(raw, phone string) string { return "" })

	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %+v", got)
	}
	for _, c := range got {
		if c.PhoneJID != "" {
			t.Fatalf("an unresolved LID must not claim a phone JID: %+v", c)
		}
	}
}
