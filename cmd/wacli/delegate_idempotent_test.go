package main

import (
	"context"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

const retryID = "3EB0RETRYID0000001"

func sendTextRequest(to string) sendDelegateRequest {
	return sendDelegateRequest{Version: sendDelegateVersion, Kind: "text", To: to, Message: "hi", AllowSelf: true, NewID: retryID, TimeoutMS: 5000}
}

// M4: the same --id twice reaches the wire once. The second call finds the
// stored message and answers already_sent.
func TestNewIDRetryDoesNotSendTwice(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"

	first, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err != nil || !first.Sent || first.ID != retryID || first.AlreadySent {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	second, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.OK || !second.Sent || !second.AlreadySent || second.ID != retryID {
		t.Fatalf("second = %+v, want ok, sent, already_sent and the same id", second)
	}
	if len(f.sendTexts) != 1 {
		t.Fatalf("the message went out %d times", len(f.sendTexts))
	}
}

// MAJ-2: the id is unique per action, so a from-me hit in any chat means the
// action was sent. The answer names the stored chat and warns; it never asks for
// another id, which would send the same text twice.
func TestNewIDHitInAnotherChatIsAlreadySentWithWarning(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	if err := a.DB().UpsertChat("5511888888888@s.whatsapp.net", "dm", "other", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "5511888888888@s.whatsapp.net", MsgID: retryID, Timestamp: time.Now(), FromMe: true, Text: "old"}); err != nil {
		t.Fatal(err)
	}
	resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest("5511999999999@s.whatsapp.net"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !resp.AlreadySent || resp.To != "5511888888888@s.whatsapp.net" || resp.TargetMismatch == "" || len(f.sendTexts) != 0 {
		t.Fatalf("resp = %+v sends=%d", resp, len(f.sendTexts))
	}
}

// The real case: the registration check rewrites 55119XXXXXXXX to 5511XXXXXXXX,
// the first send stores under the rewritten JID, and the retry carries the typed
// number. It must be recognized as sent.
func TestNewIDRetryAfterBrazilianNinthDigitRewrite(t *testing.T) {
	f := newDaemonFakeWA()
	f.rewriteOnWhatsApp = func(phone string) (types.JID, bool) {
		// +5511987654321 -> 551187654321 (the ninth digit dropped)
		if phone == "+5511987654321" {
			return types.JID{User: "551187654321", Server: types.DefaultUserServer}, true
		}
		return types.JID{}, false
	}
	a := newDaemonApp(t, f)
	typed := "5511987654321@s.whatsapp.net"

	first, err := executeDelegatedSend(context.Background(), a, sendTextRequest(typed))
	if err != nil || !first.Sent || first.AlreadySent {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	if first.To != "551187654321@s.whatsapp.net" {
		t.Fatalf("the send was not stored under the rewritten JID: %+v", first)
	}
	second, err := executeDelegatedSend(context.Background(), a, sendTextRequest(typed))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !second.AlreadySent || second.To != "551187654321@s.whatsapp.net" || second.TargetMismatch == "" {
		t.Fatalf("retry = %+v, want already_sent to the stored chat with a target_mismatch warning", second)
	}
	if len(f.sendTexts) != 1 {
		t.Fatalf("the text went out %d times", len(f.sendTexts))
	}
}

// A message from someone else with the same id does not count.
func TestNewIDMatchesOnlyOwnMessages(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if err := a.DB().UpsertChat(to, "dm", "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: to, MsgID: retryID, Timestamp: time.Now(), FromMe: false, Text: "theirs"}); err != nil {
		t.Fatal(err)
	}
	resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err != nil || resp.AlreadySent || len(f.sendTexts) != 1 {
		t.Fatalf("resp=%+v err=%v sends=%d", resp, err, len(f.sendTexts))
	}
}

func TestNoNewIDStillSendsEveryTime(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	req := sendTextRequest("5511999999999@s.whatsapp.net")
	req.NewID = ""
	for i := 0; i < 2; i++ {
		if _, err := executeDelegatedSend(context.Background(), a, req); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sendTexts) != 2 {
		t.Fatalf("sends = %d, want 2", len(f.sendTexts))
	}
}
