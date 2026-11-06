package main

import (
	"context"
	"strings"
	"testing"
)

const testGroup = "120363000000000001@g.us"

func init() {
	registerRefusal("groups", "create", "--name", "x", "--user", "5511999999999")
	registerRefusal("groups", "refresh")
	registerRefusal("groups", "info", "--jid", testGroup)
	registerRefusal("groups", "rename", "--jid", testGroup, "--name", "x")
	registerRefusal("groups", "topic", "--jid", testGroup, "--text", "x")
	registerRefusal("groups", "description", "--jid", testGroup, "--text", "x")
	registerRefusal("groups", "announce-only", "--jid", testGroup, "--on")
	registerRefusal("groups", "locked", "--jid", testGroup, "--off")
	for _, act := range []string{"add", "remove", "promote", "demote"} {
		registerRefusal("groups", "participants", act, "--jid", testGroup, "--user", "5511999999999")
	}
	registerRefusal("groups", "requests", "list", "--jid", testGroup)
	registerRefusal("groups", "requests", "approve", "--jid", testGroup, "--user", "5511999999999")
	registerRefusal("groups", "requests", "reject", "--jid", testGroup, "--user", "5511999999999")
	registerRefusal("groups", "invite", "link", "get", "--jid", testGroup)
	registerRefusal("groups", "invite", "link", "revoke", "--jid", testGroup)
	registerRefusal("groups", "join", "--code", "ABCDEF123")
	registerRefusal("groups", "leave", "--jid", testGroup)
}

func TestGroupKindValidators(t *testing.T) {
	yes := true
	cases := []struct {
		name string
		kind string
		req  sendDelegateRequest
		ok   bool
	}{
		{"create ok", groupCreateKind, sendDelegateRequest{Name: "g", Users: []string{"5511999999999"}}, true},
		{"create no name", groupCreateKind, sendDelegateRequest{}, false},
		{"create both parents", groupCreateKind, sendDelegateRequest{Name: "g", Parent: true, LinkedParent: testGroup}, false},
		{"create bad parent", groupCreateKind, sendDelegateRequest{Name: "g", LinkedParent: "1@s.whatsapp.net"}, false},
		{"rename ok", groupRenameKind, sendDelegateRequest{GroupJID: testGroup, Name: "n"}, true},
		{"rename dm jid", groupRenameKind, sendDelegateRequest{GroupJID: "1@s.whatsapp.net", Name: "n"}, false},
		{"rename blank", groupRenameKind, sendDelegateRequest{GroupJID: testGroup, Name: " "}, false},
		{"setting ok", groupSettingKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "locked", Enable: &yes}, true},
		{"setting no mode", groupSettingKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "locked"}, false},
		{"setting unknown", groupSettingKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "x", Enable: &yes}, false},
		{"participants ok", groupParticipantsKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "promote", Users: []string{"5511999999999"}}, true},
		{"participants bad action", groupParticipantsKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "ban", Users: []string{"5511999999999"}}, false},
		{"participants none", groupParticipantsKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "add"}, false},
		{"requests ok", groupRequestsKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "approve", Users: []string{"5511999999999"}}, true},
		{"requests bad action", groupRequestsKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "add", Users: []string{"5511999999999"}}, false},
		{"invite ok", groupInviteKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "revoke"}, true},
		{"invite bad", groupInviteKind, sendDelegateRequest{GroupJID: testGroup, SubAction: "x"}, false},
		{"join ok", groupJoinKind, sendDelegateRequest{Code: "ABCDEF123"}, true},
		{"join spaces", groupJoinKind, sendDelegateRequest{Code: "AB CD"}, false},
		{"join empty", groupJoinKind, sendDelegateRequest{}, false},
		{"leave ok", groupLeaveKind, sendDelegateRequest{GroupJID: testGroup}, true},
		{"leave empty", groupLeaveKind, sendDelegateRequest{}, false},
		{"info ok", groupInfoKind, sendDelegateRequest{GroupJID: testGroup}, true},
		{"refresh ok", groupRefreshKind, sendDelegateRequest{}, true},
	}
	for _, c := range cases {
		err := delegateHandlers[c.kind].validate(c.req)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
	empty := ""
	if err := delegateHandlers[groupTopicKind].validate(sendDelegateRequest{GroupJID: testGroup, Topic: &empty}); err != nil {
		t.Errorf("an empty topic clears it and must be valid: %v", err)
	}
	if delegateHandlers[groupTopicKind].validate(sendDelegateRequest{GroupJID: testGroup}) == nil {
		t.Error("a missing topic must be refused")
	}
}

func init() { validatesEmpty[groupRefreshKind] = true }

// The daemon validates again: a bad request must be refused before the app is used (the app is nil here).
func TestDaemonRevalidatesRegisteredKinds(t *testing.T) {
	_, err := executeDelegatedSend(context.Background(), nil, sendDelegateRequest{Version: sendDelegateVersion, Kind: groupParticipantsKind, GroupJID: testGroup, SubAction: "ban", Users: []string{"1"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported participants action") {
		t.Fatalf("err = %v", err)
	}
	_, err = executeDelegatedSend(context.Background(), nil, sendDelegateRequest{Version: sendDelegateVersion, Kind: forwardKind, Chat: "c", ID: "bad id", To: "t"})
	if err == nil || !strings.Contains(err.Error(), "invalid --id") {
		t.Fatalf("err = %v", err)
	}
	_, err = executeDelegatedSend(context.Background(), nil, sendDelegateRequest{Version: sendDelegateVersion, Kind: "no_such_kind"})
	if err == nil || !strings.Contains(err.Error(), "unsupported send kind") {
		t.Fatalf("err = %v", err)
	}
}

func TestGroupCommandsRejectBadInputBeforeIO(t *testing.T) {
	for _, args := range [][]string{
		{"groups", "rename", "--jid", "1@s.whatsapp.net", "--name", "x"},
		{"groups", "participants", "add", "--jid", testGroup},
		{"groups", "join", "--code", "a b"},
		{"groups", "info", "--jid", "nope"},
	} {
		err := execute(append([]string{"--store", t.TempDir()}, args...))
		if err == nil || strings.Contains(err.Error(), "refusing to connect") || strings.Contains(err.Error(), "not authenticated") {
			t.Errorf("%v: err = %v, want an input error before any I/O", args, err)
		}
	}
}

func TestGroupKindsDelegateThroughSocket(t *testing.T) {
	req, stdout := runKindViaSocket(t, `{"updated":[{"JID":"5511999999999@s.whatsapp.net","IsAdmin":true}]}`, "",
		"groups", "participants", "promote", "--jid", testGroup, "--user", "5511999999999")
	if req.Kind != groupParticipantsKind || req.SubAction != "promote" || req.GroupJID != testGroup || len(req.Users) != 1 {
		t.Fatalf("req = %+v", req)
	}
	if !strings.Contains(stdout, "5511999999999@s.whatsapp.net") {
		t.Fatalf("stdout = %q", stdout)
	}
	req, stdout = runKindViaSocket(t, `{"jid":"`+testGroup+`","link":"https://chat.whatsapp.com/AAA","revoked":true}`, "",
		"groups", "invite", "link", "revoke", "--jid", testGroup)
	if req.Kind != groupInviteKind || req.SubAction != "revoke" || !strings.Contains(stdout, `"revoked":true`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
	req, stdout = runKindViaSocket(t, `{"groups":7}`, "", "groups", "refresh")
	if req.Kind != groupRefreshKind || !strings.Contains(stdout, `"groups":7`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+testGroup+`","joined":true}`, "", "groups", "join", "--code", "ABCDEF123")
	if req.Kind != groupJoinKind || req.Code != "ABCDEF123" {
		t.Fatalf("req = %+v", req)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+testGroup+`","setting":"locked","enabled":true}`, "", "groups", "locked", "--jid", testGroup, "--on")
	if req.Kind != groupSettingKind || req.SubAction != "locked" || req.Enable == nil || !*req.Enable {
		t.Fatalf("req = %+v", req)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+testGroup+`","topic":""}`, "", "groups", "topic", "--jid", testGroup, "--text", "")
	if req.Kind != groupTopicKind || req.Topic == nil || *req.Topic != "" {
		t.Fatalf("an empty topic must reach the daemon as an empty string: %+v", req)
	}
}
