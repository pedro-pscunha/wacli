package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
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
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{ChatJID: "5511888888888@s.whatsapp.net", MsgID: retryID, Timestamp: time.Now(), FromMe: true, Text: "hi"}); err != nil {
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
	// The same typed number again is a plain retry: no warning. The warning is
	// for a different typed recipient (TestTargetMismatchOnlyWhenTheTypedRecipientDiffers).
	if !second.AlreadySent || second.To != "551187654321@s.whatsapp.net" || second.TargetMismatch != "" {
		t.Fatalf("retry = %+v, want already_sent to the stored chat and no warning", second)
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

// m-h: a send whose outcome is unknown (the ack wait was cut) leaves a pending
// row. The retry with the same id answers resultado_incerto and does not resend.
func TestUnknownOutcomeBlocksARetryWithResultadoIncerto(t *testing.T) {
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{errors.New("send timed out after 2m0s")}
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"

	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err == nil {
		t.Fatal("the first attempt should fail with the timeout")
	}
	resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err == nil || !strings.Contains(err.Error(), "resultado_incerto") {
		t.Fatalf("retry err = %v, want resultado_incerto", err)
	}
	if !resp.OutcomeUnknown || resp.ID != retryID {
		t.Fatalf("retry resp = %+v", resp)
	}
	if len(f.sendTexts) != 0 || len(f.idsSeen) != 1 {
		t.Fatalf("the retry went on the wire: sends=%d attempts=%d", len(f.sendTexts), len(f.idsSeen))
	}
}

// A failure before the wire is definite: the row goes away and the retry sends.
func TestCleanFailureDoesNotBlockTheRetry(t *testing.T) {
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{errors.New("not connected")}
	f.errBeforeWire = true
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err == nil {
		t.Fatal("the first attempt should fail")
	}
	resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err != nil || !resp.Sent || resp.AlreadySent {
		t.Fatalf("retry = %+v err=%v", resp, err)
	}
	if len(f.sendTexts) != 1 {
		t.Fatalf("sends = %d", len(f.sendTexts))
	}
}

// A success leaves only a "done" record (the typed recipient), which never blocks
// a new attempt: the stored message is the proof, and it answers first.
func TestSuccessfulSendLeavesOnlyANonBlockingRecord(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest("5511999999999@s.whatsapp.net")); err != nil {
		t.Fatal(err)
	}
	ps, ok, err := a.DB().GetPendingSend(retryID)
	if err != nil || !ok || ps.State != store.PendingSendDone {
		t.Fatalf("record = %+v ok=%v err=%v, want a done record", ps, ok, err)
	}
}

// ---- review round 3: m-1, m-2, m-3 -------------------------------------------

func textRequestWith(to, message string) sendDelegateRequest {
	r := sendTextRequest(to)
	r.Message = message
	return r
}

// m-1: classification is by phase. After the dispatch mark, a dropped websocket
// is unknown; the server's own error reply is definite.
func TestPhaseClassificationAfterDispatch(t *testing.T) {
	if !wa.ErrorAfterDispatchIsUnknown(&whatsmeow.DisconnectedError{Action: "message send"}) {
		t.Error("a websocket that dropped before the send returned must be unknown")
	}
	if !wa.ErrorAfterDispatchIsUnknown(whatsmeow.ErrNotConnected) {
		t.Error("ErrNotConnected after the frame was written must be unknown")
	}
	if !wa.ErrorAfterDispatchIsUnknown(context.DeadlineExceeded) {
		t.Error("a timeout after dispatch must be unknown")
	}
	if wa.ErrorAfterDispatchIsUnknown(fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError)) {
		t.Error("the server's own error reply is definite")
	}
	if wa.ErrorAfterDispatchIsUnknown(nil) {
		t.Error("no error is not unknown")
	}
}

// A dropped websocket after the frame went out keeps the row, with no text in
// the error that the old classifier could match.
func TestDroppedWebsocketAfterDispatchBlocksTheRetry(t *testing.T) {
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{&whatsmeow.DisconnectedError{Action: "message send"}}
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err == nil {
		t.Fatal("the first attempt should fail")
	}
	_, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	var unknown *outcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("retry err = %v, want resultado_incerto", err)
	}
	if len(f.sendTexts) != 0 {
		t.Fatalf("the retry went on the wire: %v", f.sendTexts)
	}
}

// A failure before the wire that merely mentions a timeout is definite: the
// phase, not the text, decides. The old classifier locked this id for a week.
func TestErrorBeforeDispatchMentioningTimeoutIsDefinite(t *testing.T) {
	f := newDaemonFakeWA()
	// "info query timed out" is retryable, so the send is tried twice: two failures.
	f.sendTextErrs = []error{
		errors.New("info query timed out; reconnect failed: daemon not connected, not sent"),
		errors.New("info query timed out; reconnect failed: daemon not connected, not sent"),
	}
	f.errBeforeWire = true
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err == nil {
		t.Fatal("the first attempt should fail")
	}
	resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to))
	if err != nil || !resp.Sent || resp.AlreadySent {
		t.Fatalf("retry = %+v err=%v, want a plain send", resp, err)
	}
}

// The server's own error reply after dispatch is a definite failure.
func TestServerErrorAfterDispatchIsDefinite(t *testing.T) {
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError)}
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err == nil {
		t.Fatal("the first attempt should fail")
	}
	if resp, err := executeDelegatedSend(context.Background(), a, sendTextRequest(to)); err != nil || resp.AlreadySent {
		t.Fatalf("retry = %+v err=%v", resp, err)
	}
}

// m-1: a send that went out but whose store write failed keeps a "sent" row; the
// retry answers already_sent, never a second send.
func TestStoreWarningKeepsASentRowAndTheRetryIsAlreadySent(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	ctx, phase := wa.WithSendPhase(context.Background())
	finish, early, err := beginIDSend(a, textRequestWith(to, "hi"), phase)
	if err != nil || early != nil {
		t.Fatalf("begin: %v %v", err, early)
	}
	wa.MarkDispatched(ctx)
	finish(nil, "message update: disk full", "551187654321@s.whatsapp.net") // sent, store failed

	resp, err := executeDelegatedSend(context.Background(), a, textRequestWith(to, "hi"))
	if err != nil || !resp.AlreadySent || resp.To != "551187654321@s.whatsapp.net" {
		t.Fatalf("retry = %+v err=%v, want already_sent to the stored chat", resp, err)
	}
	if len(f.sendTexts) != 0 {
		t.Fatalf("the retry went on the wire: %v", f.sendTexts)
	}
}

// m-2: OutcomeUnknown and the id cross the socket and become a typed error.
func TestOutcomeUnknownCrossesTheSocketAndHasItsOwnExitCode(t *testing.T) {
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{&whatsmeow.DisconnectedError{Action: "message send"}}
	fx := newDaemonFixture(t, f)
	req := sendTextRequest("5511999999999@s.whatsapp.net")
	if first := fx.call(t, req); first.OK {
		t.Fatalf("the first attempt should fail: %+v", first)
	}
	second := fx.call(t, req)
	if second.OK || !second.OutcomeUnknown || second.ID != retryID || !strings.Contains(second.Error, outcomeUnknownCode) {
		t.Fatalf("second over the socket = %+v, want the typed resultado_incerto answer", second)
	}
	if !strings.Contains(second.Error, "confira no celular") || !strings.Contains(second.Error, "--id novo") {
		t.Fatalf("advice = %q", second.Error)
	}
	err := error(&outcomeUnknownError{resp: second})
	if exitCodeFor(err) != exitCodeOutcomeUnknown || exitCodeFor(errors.New("x")) != 1 {
		t.Fatalf("exit codes: %d %d", exitCodeFor(err), exitCodeFor(errors.New("x")))
	}
}

func TestOutcomeUnknownCLIPrintsTheTypedFields(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	server := startPresenceDelegateTestSocket(t, storeDir, func(sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: false, OutcomeUnknown: true, ID: retryID, Error: outcomeUnknownCode + ": " + outcomeUnknownAdvice(retryID)}
	})
	defer server.stop()
	_, stderr, runErr := runPresenceDelegateHelper(t, []string{"--store", storeDir, "--json", "--timeout", "750ms", "send", "text", "--to", "+15551234567", "--message", "hi", "--id", retryID})
	if runErr == nil {
		t.Fatal("resultado_incerto must be a failure")
	}
	for _, want := range []string{`"outcome_unknown":true`, `"code":"resultado_incerto"`, `"id":"` + retryID + `"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %s: %q", want, stderr)
		}
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && ee.ExitCode() != exitCodeOutcomeUnknown {
		t.Errorf("exit code = %d, want %d", ee.ExitCode(), exitCodeOutcomeUnknown)
	}
}

// m-2: `pending-sends clear` releases a wrongly blocked id.
func TestPendingSendsClearReleasesAnId(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	store := t.TempDir()
	f := newDaemonFakeWA()
	a := newDaemonAppAt(t, f, store)
	if _, ins, err := a.DB().BeginPendingSend(storeRow(retryID, "text", "hi")); err != nil || !ins {
		t.Fatalf("seed: %v %v", ins, err)
	}
	a.Close()

	if err := execute([]string{"--store", store, "pending-sends", "clear", "--id", retryID}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	b := newDaemonAppAt(t, f, store)
	if _, ins, _ := b.DB().BeginPendingSend(storeRow(retryID, "text", "hi")); !ins {
		t.Fatal("the id is still blocked after clear")
	}
	if err := execute([]string{"--store", store, "pending-sends", "clear"}); err == nil {
		t.Fatal("clear without --id must fail")
	}
}

func storeRow(id, kind, text string) store.PendingSend {
	return store.PendingSend{MsgID: id, ChatJID: "x", TypedTo: "x", Kind: kind, ContentHash: store.ContentHash(kind, text)}
}

// m-3: the same id for a DIFFERENT message is refused, not reported as sent.
func TestReusedIDForAnotherMessageIsRefused(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	to := "5511999999999@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, textRequestWith(to, "first digest")); err != nil {
		t.Fatal(err)
	}
	// The same id, the same chat, other text: a collision, not a retry.
	_, err := executeDelegatedSend(context.Background(), a, textRequestWith(to, "second digest"))
	if err == nil || !strings.Contains(err.Error(), "id_reused") {
		t.Fatalf("err = %v, want id_reused", err)
	}
	// And in another chat.
	_, err = executeDelegatedSend(context.Background(), a, textRequestWith("5511777777777@s.whatsapp.net", "first digest to someone else"))
	if err == nil || !strings.Contains(err.Error(), "id_reused") {
		t.Fatalf("other chat, other text: err = %v, want id_reused", err)
	}
	if len(f.sendTexts) != 1 {
		t.Fatalf("sends = %d, want only the first", len(f.sendTexts))
	}
}

// m-3: target_mismatch fires only when the typed recipient itself differs from
// the one the first attempt typed, not on every retry of a rewritten number.
func TestTargetMismatchOnlyWhenTheTypedRecipientDiffers(t *testing.T) {
	f := newDaemonFakeWA()
	f.rewriteOnWhatsApp = func(phone string) (types.JID, bool) {
		if phone == "+5511987654321" {
			return types.JID{User: "551187654321", Server: types.DefaultUserServer}, true
		}
		return types.JID{}, false
	}
	a := newDaemonApp(t, f)
	typed := "5511987654321@s.whatsapp.net"
	if _, err := executeDelegatedSend(context.Background(), a, textRequestWith(typed, "hi")); err != nil {
		t.Fatal(err)
	}
	// The same typed number again: a plain retry, no warning. The first attempt's
	// typed recipient must still be known, so the success keeps it.
	retry, err := executeDelegatedSend(context.Background(), a, textRequestWith(typed, "hi"))
	if err != nil || !retry.AlreadySent || retry.TargetMismatch != "" {
		t.Fatalf("same typed number: %+v err=%v, want already_sent with no warning", retry, err)
	}
	// Another number under the same id and text: warn.
	other, err := executeDelegatedSend(context.Background(), a, textRequestWith("5511666666666@s.whatsapp.net", "hi"))
	if err != nil || !other.AlreadySent || other.TargetMismatch == "" {
		t.Fatalf("a different typed number: %+v err=%v, want the warning", other, err)
	}
}

// m-6: the commands that send on their own connection write the pending row
// too, so a cut ack there leaves the same trace the daemon path leaves.
func TestLocalGuardWritesAndKeepsThePendingRow(t *testing.T) {
	f := newDaemonFakeWA()
	a := newDaemonApp(t, f)
	flags := &rootFlags{}
	to := "5511999999999@s.whatsapp.net"

	sendCtx, finish, done, err := localIDGuard(context.Background(), flags, a, "text", to, "hi", retryID)
	if err != nil || done {
		t.Fatalf("guard: done=%v err=%v", done, err)
	}
	if ps, ok, _ := a.DB().GetPendingSend(retryID); !ok || ps.State != store.PendingSendInFlight {
		t.Fatalf("no in-flight row while the send runs: %+v ok=%v", ps, ok)
	}
	// The ack wait was cut after dispatch: the row must stay. The real client
	// marks the dispatch right before it hands the frame to whatsmeow.
	wa.MarkDispatched(sendCtx)
	finish(context.DeadlineExceeded, "")
	if ps, ok, _ := a.DB().GetPendingSend(retryID); !ok || ps.State != store.PendingSendInFlight {
		t.Fatalf("an unknown outcome must keep the row: %+v ok=%v", ps, ok)
	}
	// A cancellation BEFORE the dispatch mark is definite and releases the id.
	_, fin2, done2, err2 := localIDGuard(context.Background(), flags, a, "text", to, "hi", "3EB0OTHERID0000003")
	if done2 || err2 != nil {
		t.Fatalf("second id guard: %v %v", done2, err2)
	}
	fin2(context.Canceled, "")
	if _, ok, _ := a.DB().GetPendingSend("3EB0OTHERID0000003"); ok {
		t.Fatal("a cancel before dispatch kept the row")
	}
	// A second local attempt is answered resultado_incerto.
	_, _, done, err = localIDGuard(context.Background(), flags, a, "text", to, "hi", retryID)
	var unknown *outcomeUnknownError
	if !done || !errors.As(err, &unknown) {
		t.Fatalf("second local attempt: done=%v err=%v, want resultado_incerto", done, err)
	}
	// No id, no guard.
	if _, _, done, err := localIDGuard(context.Background(), flags, a, "text", to, "hi", ""); done || err != nil {
		t.Fatalf("no --id must not guard: %v %v", done, err)
	}
}
