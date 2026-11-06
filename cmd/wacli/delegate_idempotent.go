package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
)

// kindStoresSentMessage lists the kinds whose sent message lands in wacli.db
// under the new id, so a stored message proves the send went through. Edit and
// revoke store nothing under their own id, so they are not here.
var kindStoresSentMessage = map[string]bool{
	"text": true, "file": true, "voice": true, "sticker": true, "location": true,
	"poll": true, "react": true, forwardKind: true,
}

// One id per message. The id is chosen by the caller and names one message: one
// action sent to one recipient. A fan-out to several chats needs one id per
// chat, and an id derived from a template or a rule must include the run. The
// guard below answers a repeated id as "the same message again" only when the
// kind and text match; a different message under a spent id is refused.

const (
	outcomeUnknownCode = "resultado_incerto"
	idReusedCode       = "id_reused"
)

// requestContentHash fingerprints the request the way the stored row does.
func requestContentHash(req sendDelegateRequest) string {
	text := req.Message
	if text == "" {
		text = req.Caption
	}
	if text == "" {
		text = req.Question
	}
	if text == "" {
		text = req.Name
	}
	return store.ContentHash(storedKindOf(req.Kind), text)
}

// storedKindOf maps a delegate kind to the media_type a stored row carries, so a
// request and its stored message compare equal.
func storedKindOf(kind string) string {
	switch kind {
	case "text":
		return "text"
	case "react":
		return "reaction"
	case forwardKind:
		return "forward"
	}
	return kind
}

func storedRefHash(ref store.OwnMessageRef, kind string) string {
	return store.ContentHash(storedKindOf(kind), ref.Text)
}

// alreadySentResponse makes a retry with the same --id safe. Before anything
// goes on the wire it looks the id up among this account's sent messages.
//
//   - A hit with the same kind and text means this message was sent: answer
//     already_sent with the stored chat and send nothing. The stored chat can
//     differ from the typed recipient, because a send stores under the JID the
//     registration check returned (55119XXXXXXXX becomes 5511XXXXXXXX). That is
//     not a warning when the typed recipient is the one the first attempt
//     typed; target_mismatch fires only when the typed recipient itself differs.
//   - A hit with a different kind or text is another message under a spent id:
//     refuse with id_reused, because answering "sent" would silently drop it.
//
// done is true when the request is answered here.
func alreadySentResponse(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, bool, error) {
	if req.NewID == "" || !kindStoresSentMessage[req.Kind] || a == nil || a.DB() == nil {
		return sendDelegateResponse{}, false, nil
	}
	ref, found, err := a.DB().FindOwnMessageRef(req.NewID)
	if err != nil {
		return sendDelegateResponse{}, false, fmt.Errorf("look up message id %s: %w", req.NewID, err)
	}
	if !found {
		return pendingSentResponse(ctx, a, req)
	}
	if reusedForAnotherMessage(req, ref) {
		return sendDelegateResponse{}, false, fmt.Errorf("%s: message id %s already names a different message in %s ; use one id per message (action and recipient)", idReusedCode, req.NewID, ref.ChatJID)
	}
	resp := sendDelegateResponse{OK: true, Sent: true, AlreadySent: true, To: ref.ChatJID, ID: req.NewID}
	resp.TargetMismatch = targetMismatch(ctx, a, req, ref.ChatJID)
	if req.Kind == forwardKind {
		raw, _ := json.Marshal(forwardResult{Forwarded: true, To: ref.ChatJID, ID: req.NewID, Source: req.ID})
		resp.Data = raw
	}
	return resp, true, nil
}

// pendingSentResponse covers a send that went out but whose local store write
// failed: no messages row exists, but the pending row says "sent".
func pendingSentResponse(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, bool, error) {
	ps, ok, err := a.DB().GetPendingSend(req.NewID)
	if err != nil || !ok || ps.State != store.PendingSendSent {
		return sendDelegateResponse{}, false, err
	}
	if ps.Kind != storedKindOf(req.Kind) || ps.ContentHash != requestContentHash(req) {
		return sendDelegateResponse{}, false, fmt.Errorf("%s: message id %s already names a different message in %s; use one id per message (action and recipient)", idReusedCode, req.NewID, ps.ChatJID)
	}
	resp := sendDelegateResponse{OK: true, Sent: true, AlreadySent: true, To: ps.ChatJID, ID: req.NewID}
	if typed := strings.TrimSpace(req.To); typed != "" && ps.TypedTo != "" && typed != ps.TypedTo {
		resp.TargetMismatch = fmt.Sprintf("message id %s was sent for %s, not for %s", req.NewID, ps.TypedTo, typed)
	}
	return resp, true, nil
}

// reusedForAnotherMessage compares the stored row with the request. Only rows
// that carry text to compare can prove a reuse; for the media kinds (the stored
// caption can differ from the file) only the kind is compared.
func reusedForAnotherMessage(req sendDelegateRequest, ref store.OwnMessageRef) bool {
	switch req.Kind {
	case "text":
		return strings.TrimSpace(ref.Text) != strings.TrimSpace(req.Message) || strings.TrimSpace(ref.MediaType) != ""
	case "react", forwardKind:
		return false
	default:
		return false
	}
}

func targetMismatchText(newID, storedChat, typed string) string {
	return fmt.Sprintf("message id %s was sent to %s, not to the typed recipient %s", newID, storedChat, typed)
}

// targetMismatch warns when the typed recipient differs from the one the first
// attempt typed. The stored chat alone proves nothing: it is the rewritten JID.
func targetMismatch(ctx context.Context, a *app.App, req sendDelegateRequest, storedChat string) string {
	typed := strings.TrimSpace(req.To)
	if typed == "" {
		return ""
	}
	if ps, ok, _ := a.DB().GetPendingSend(req.NewID); ok && ps.TypedTo != "" {
		if ps.TypedTo == typed {
			return ""
		}
		return targetMismatchText(req.NewID, storedChat, typed)
	}
	if chatMatchesTarget(ctx, a, storedChat, typed) {
		return ""
	}
	return targetMismatchText(req.NewID, storedChat, typed)
}

// chatMatchesTarget reports whether the stored chat is the typed recipient or
// its PN/LID twin.
func chatMatchesTarget(ctx context.Context, a *app.App, storedChat, target string) bool {
	chats, err := messageChatJIDFilter(ctx, a, target)
	if err != nil {
		return false
	}
	for _, c := range chats {
		if c == storedChat {
			return true
		}
	}
	return false
}

// beginIDSend is the second half of the retry guard, after the stored-message
// check. It records the send with a pending row before anything goes on the
// wire, and returns the finish function that closes it from the send's result
// and the phase it reached.
//
// A retry that finds a row left by an attempt that never finished answers
// resultado_incerto and does not resend: that attempt may have reached the
// recipient. A row in state "sent" is already_sent (handled before this).
func beginIDSend(a *app.App, req sendDelegateRequest, phase *wa.SendPhase) (finish func(sendErr error, storeWarning string, storedChat string), early *sendDelegateResponse, err error) {
	noop := func(error, string, string) {}
	if req.NewID == "" || !kindStoresSentMessage[req.Kind] || a == nil || a.DB() == nil {
		return noop, nil, nil
	}
	chat := strings.TrimSpace(req.To)
	if chat == "" {
		chat = strings.TrimSpace(req.Chat)
	}
	row := store.PendingSend{
		MsgID: req.NewID, ChatJID: chat, TypedTo: chat,
		Kind: storedKindOf(req.Kind), ContentHash: requestContentHash(req),
	}
	existing, inserted, err := a.DB().BeginPendingSend(row)
	if err != nil {
		return nil, nil, fmt.Errorf("record pending send %s: %w", req.NewID, err)
	}
	if !inserted && existing.State == store.PendingSendDone {
		// A finished send with this id whose stored message is gone (purged, or
		// the store was rebuilt): the record no longer proves anything. Start over.
		_, _ = a.DB().FinishPendingSend(req.NewID)
		existing, inserted, err = a.DB().BeginPendingSend(row)
		if err != nil {
			return nil, nil, fmt.Errorf("record pending send %s: %w", req.NewID, err)
		}
	}
	if !inserted {
		if existing.Kind != row.Kind || existing.ContentHash != row.ContentHash {
			return nil, nil, fmt.Errorf("%s: message id %s already names a different message in %s; use one id per message (action and recipient)", idReusedCode, req.NewID, existing.ChatJID)
		}
		resp := sendDelegateResponse{
			OK: false, ID: req.NewID, OutcomeUnknown: true,
			Error: outcomeUnknownCode + ": " + outcomeUnknownAdvice(req.NewID),
		}
		return nil, &resp, nil
	}
	return func(sendErr error, storeWarning, storedChat string) {
		switch {
		case sendErr == nil && storeWarning != "":
			// It went out, but no messages row will say so: remember that.
			_ = a.DB().MarkPendingSendSent(req.NewID, storedChat)
		case sendErr == nil:
			// Keep the typed recipient: a later retry compares against it.
			_ = a.DB().CompletePendingSend(req.NewID, storedChat)
		case phase != nil && phase.Dispatched() && wa.ErrorAfterDispatchIsUnknown(sendErr):
			// Past the point of no return and the server did not answer: keep the row.
		case phase == nil && sendOutcomeUnknownByContext(sendErr):
			// No phase information (a kind that does not mark one): fall back to
			// the one signal that is reliable, a context ended after the start.
		default:
			_, _ = a.DB().FinishPendingSend(req.NewID)
		}
	}, nil, nil
}

// outcomeUnknownAdvice is the text a person reads. The local store cannot show
// the message (it was never stored), so the check is on the phone.
func outcomeUnknownAdvice(id string) string {
	return "an earlier attempt with --id " + id + " started and its result is unknown; confira no celular; se nao estiver la, envie de novo com um --id novo (or run `wacli pending-sends clear --id " + id + "` if you are sure it never left)"
}

// sendOutcomeUnknownByContext is the fallback for a send without phase marks.
func sendOutcomeUnknownByContext(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// localIDGuard is the retry guard for a command that sends on its own
// connection. It runs the stored-message check, then writes the pending row, so
// a crash or a cut ack leaves the same trace the daemon path leaves. It returns
// the context to send with (it carries the phase mark), and a finish func to
// call with the send's result. done is true when the request was answered here
// (already sent, or an unknown earlier attempt) and the command must return err.
// text is what the message says, for the content check against an earlier
// message under the same id.
func localIDGuard(ctx context.Context, flags *rootFlags, a *app.App, kind, to, text, newID string) (sendCtx context.Context, finish func(error, string), done bool, err error) {
	if newID == "" {
		return ctx, func(error, string) {}, false, nil
	}
	req := sendDelegateRequest{Kind: kind, To: to, NewID: newID, Message: text}
	if resp, answered, aerr := alreadySentResponse(ctx, a, req); aerr != nil {
		return ctx, nil, true, aerr
	} else if answered {
		return ctx, nil, true, writeDelegatedSendOutput(flags, kind, resp)
	}
	sendCtx, phase := wa.WithSendPhase(ctx)
	fin, early, berr := beginIDSend(a, req, phase)
	if berr != nil {
		return ctx, nil, true, berr
	}
	if early != nil {
		return ctx, nil, true, &outcomeUnknownError{resp: *early}
	}
	return sendCtx, func(sendErr error, storeWarning string) { fin(sendErr, storeWarning, "") }, false, nil
}

// addSendMetaFields puts the retry and mismatch facts into a JSON body, the
// same keys `send` prints.
func addSendMetaFields(body map[string]any, m sendMeta) {
	if m.AlreadySent {
		body["already_sent"] = true
	}
	if m.TargetMismatch != "" {
		body["target_mismatch"] = m.TargetMismatch
	}
	if m.IDRequested != "" {
		body["id_requested"] = m.IDRequested
		body["warning"] = "the sync daemon ignored --new-id; the message was sent under the id above"
	}
}

// outcomeUnknownError is returned when an earlier attempt with the same --id
// started and its result is unknown. It carries the typed response so the
// daemon keeps it on the wire and the CLI can give it its own exit code and
// JSON field.
type outcomeUnknownError struct{ resp sendDelegateResponse }

func (e *outcomeUnknownError) Error() string { return e.resp.Error }

// exitCodeOutcomeUnknown is the process exit status for resultado_incerto, so a
// caller can tell "check the phone" from an ordinary failure (1).
const exitCodeOutcomeUnknown = 75
