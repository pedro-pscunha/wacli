package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/app"
)

// kindStoresSentMessage lists the kinds whose sent message lands in wacli.db
// under the new id, so a stored message proves the send went through. Edit and
// revoke store nothing under their own id, so they are not here.
var kindStoresSentMessage = map[string]bool{
	"text": true, "file": true, "voice": true, "sticker": true, "location": true,
	"poll": true, "react": true, forwardKind: true,
}

// alreadySentResponse makes a retry with the same --id safe. Before anything
// goes on the wire it looks the id up among this account's sent messages. The
// id is chosen by the caller, once per action, so a from-me hit ANYWHERE means
// this action was already sent: answer "already sent" with the stored chat and
// send nothing. The stored chat can differ from the typed recipient, because a
// send stores under the JID WhatsApp's registration check returned (a Brazilian
// 55119XXXXXXXX becomes 5511XXXXXXXX). Then the answer carries a
// target_mismatch warning; it never tells the caller to pick another id, since
// a fresh id would send the same text twice. done is true when the request is
// answered here.
func alreadySentResponse(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, bool, error) {
	if req.NewID == "" || !kindStoresSentMessage[req.Kind] || a == nil || a.DB() == nil {
		return sendDelegateResponse{}, false, nil
	}
	foundChat, found, err := a.DB().FindOwnMessage(req.NewID)
	if err != nil {
		return sendDelegateResponse{}, false, fmt.Errorf("look up message id %s: %w", req.NewID, err)
	}
	if !found {
		return sendDelegateResponse{}, false, nil
	}
	resp := sendDelegateResponse{OK: true, Sent: true, AlreadySent: true, To: foundChat, ID: req.NewID}
	if target := strings.TrimSpace(req.To); target != "" && !chatMatchesTarget(ctx, a, foundChat, target) {
		resp.TargetMismatch = fmt.Sprintf("message id %s was sent to %s, not to the typed recipient %s", req.NewID, foundChat, target)
	}
	if req.Kind == forwardKind {
		raw, _ := json.Marshal(forwardResult{Forwarded: true, To: foundChat, ID: req.NewID, Source: req.ID})
		resp.Data = raw
	}
	return resp, true, nil
}

// chatMatchesTarget reports whether the stored chat is the typed recipient or
// its PN/LID twin. A false answer only adds a warning.
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

// errSendOutcomeUnknown marks a send whose result nobody knows: an earlier
// attempt with the same --id started, and neither a stored message nor a clean
// failure says how it ended. The caller must check the chat before resending.
const outcomeUnknownCode = "resultado_incerto"

// beginIDSend is the second half of the retry guard, after the stored-message
// check. It records the send with a pending row before anything goes on the
// wire. A retry that finds a row left by an attempt that never finished answers
// resultado_incerto and does not resend: that attempt may have reached the
// recipient, and the same id would make WhatsApp's own deduplication the only
// protection.
//
// finish must be called with the send's error. A clean failure before the wire
// (validation, not connected) removes the row so the caller can retry at once.
// A success removes it too, since the stored message now proves the send. An
// error that leaves the outcome unknown keeps the row.
func beginIDSend(a *app.App, req sendDelegateRequest) (finish func(error), early *sendDelegateResponse, err error) {
	if req.NewID == "" || !kindStoresSentMessage[req.Kind] || a == nil || a.DB() == nil {
		return func(error) {}, nil, nil
	}
	chat := strings.TrimSpace(req.To)
	if chat == "" {
		chat = strings.TrimSpace(req.Chat)
	}
	state, inserted, err := a.DB().BeginPendingSend(req.NewID, chat)
	if err != nil {
		return nil, nil, fmt.Errorf("record pending send %s: %w", req.NewID, err)
	}
	if !inserted {
		resp := sendDelegateResponse{
			OK: false, Error: outcomeUnknownCode + ": an earlier attempt with --id " + req.NewID +
				" started and did not finish (state " + state + "); check the chat for the message before sending again",
			OutcomeUnknown: true, ID: req.NewID,
		}
		return nil, &resp, nil
	}
	return func(sendErr error) {
		if sendErr == nil || !sendOutcomeUnknown(sendErr) {
			_ = a.DB().FinishPendingSend(req.NewID)
		}
	}, nil, nil
}

// sendOutcomeUnknown reports whether an error leaves open that the message
// reached WhatsApp: a timeout or cancellation after dispatch. Errors raised
// before the wire (bad input, no connection) are definite.
func sendOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timed out") || strings.Contains(msg, "may still have gone through") || strings.Contains(msg, "timeout")
}

// localAlreadySent is the same check for a command that runs on its own
// connection. It prints the stored send like a fresh one and returns true.
func localAlreadySent(ctx context.Context, flags *rootFlags, a *app.App, kind, to, newID string) (bool, error) {
	resp, done, err := alreadySentResponse(ctx, a, sendDelegateRequest{Kind: kind, To: to, NewID: newID})
	if err != nil || !done {
		return false, err
	}
	return true, writeDelegatedSendOutput(flags, kind, resp)
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
