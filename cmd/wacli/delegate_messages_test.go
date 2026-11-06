package main

import (
	"strings"
	"testing"
)

func init() {
	registerRefusal("messages", "forward", "--chat", "5511999999999", "--id", "ABCDEFGH12", "--to", "5511888888888")
	registerRefusal("messages", "revoke", "--chat", "5511999999999", "--id", "ABCDEFGH12")
	registerRefusal("messages", "delete", "--chat", "5511999999999", "--id", "ABCDEFGH12")
	registerRefusal("messages", "delete", "--chat", "5511999999999", "--id", "ABCDEFGH12", "--for-me")
}

func TestMessageKindValidators(t *testing.T) {
	ok := sendDelegateRequest{Chat: "c", ID: "ABCDEFGH12", To: "t"}
	if err := validateForward(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []sendDelegateRequest{{ID: "x", To: "t"}, {Chat: "c", To: "t"}, {Chat: "c", ID: "x"}, {Chat: "c", ID: "a b", To: "t"}, {Chat: "c", ID: "x", To: "t", Pick: -1}} {
		if validateForward(bad) == nil {
			t.Errorf("forward accepted %+v", bad)
		}
	}
	if validateRevoke(sendDelegateRequest{Chat: "c"}) == nil || validateRevoke(sendDelegateRequest{ID: "x"}) == nil {
		t.Error("revoke accepts a missing field")
	}
	if validateDeleteForMe(sendDelegateRequest{Chat: "c", ID: strings.Repeat("a", 200)}) == nil {
		t.Error("delete-for-me accepts a 200-character id")
	}
}

func TestForwardDelegatesWithNewIDAndReturnsProducedID(t *testing.T) {
	const want = "3EB0ABCDEF0123456789"
	req, stdout := runKindViaSocket(t, `{"forwarded":true,"to":"5511@s.whatsapp.net","id":"`+want+`","source":"SRC"}`, want,
		"messages", "forward", "--chat", "5511999999999", "--id", "SRC12345", "--to", "5511888888888", "--new-id", want)
	if req.Kind != forwardKind || req.Chat != "5511999999999" || req.ID != "SRC12345" || req.To != "5511888888888" || req.NewID != want {
		t.Fatalf("request = %+v", req)
	}
	if !strings.Contains(stdout, `"id":"`+want+`"`) || !strings.Contains(stdout, `"forwarded":true`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRevokeAndDeleteDelegate(t *testing.T) {
	req, stdout := runKindViaSocket(t, `{"revoked":true,"to":"5511@s.whatsapp.net","id":"REVOKEID123","target":"ABCDEFGH12"}`, "REVOKEID123",
		"messages", "revoke", "--chat", "5511999999999", "--id", "ABCDEFGH12")
	if req.Kind != revokeKind || req.Strict || !strings.Contains(stdout, `"id":"REVOKEID123"`) {
		t.Fatalf("revoke req=%+v stdout=%q", req, stdout)
	}
	req, _ = runKindViaSocket(t, `{"revoked":true,"to":"x","id":"REVOKEID123","target":"ABCDEFGH12"}`, "REVOKEID123",
		"messages", "delete", "--chat", "5511999999999", "--id", "ABCDEFGH12")
	if req.Kind != revokeKind || !req.Strict {
		t.Fatalf("delete (everyone) must use the strict revoke kind: %+v", req)
	}
	req, stdout = runKindViaSocket(t, `{"deleted_for_me":true,"to":"x","target":"ABCDEFGH12","deleted_media":true}`, "",
		"messages", "delete", "--chat", "5511999999999", "--id", "ABCDEFGH12", "--for-me", "--delete-media")
	if req.Kind != deleteForMeKind || !req.DeleteMedia || !strings.Contains(stdout, `"deleted_media":true`) {
		t.Fatalf("delete-for-me req=%+v stdout=%q", req, stdout)
	}
}

func TestMessageKindsRejectBadInputBeforeIO(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "messages", "forward", "--chat", "c", "--id", "bad id", "--to", "t"})
	if err == nil || !strings.Contains(err.Error(), "invalid --id") {
		t.Fatalf("err = %v", err)
	}
	err = execute([]string{"--store", t.TempDir(), "messages", "forward", "--chat", "c", "--id", "ABCDEFGH12", "--to", "t", "--new-id", "no good"})
	if err == nil || !strings.Contains(err.Error(), "invalid message id") {
		t.Fatalf("err = %v", err)
	}
}

// m-e: the mismatch and retry facts survive the typed-result path.
func TestForwardKeepsIDMismatchAndAlreadySentFromTheDaemon(t *testing.T) {
	// An old daemon that ignored --new-id: it sent under another id.
	_, stdout := runKindViaSocket(t, `{"forwarded":true,"to":"5511@s.whatsapp.net","id":"OLDRANDOMID1","source":"SRC"}`, "OLDRANDOMID1",
		"messages", "forward", "--chat", "5511999999999", "--id", "SRC12345", "--to", "5511888888888", "--new-id", "3EB0ABCDEF0123456789")
	for _, want := range []string{`"id":"OLDRANDOMID1"`, `"id_requested":"3EB0ABCDEF0123456789"`, `"warning"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("forward output lacks %s: %q", want, stdout)
		}
	}
}
