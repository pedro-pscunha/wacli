package main

import (
	"context"
	"encoding/json"
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

// localAlreadySent is the same check for a command that runs on its own
// connection. It prints the stored send like a fresh one and returns true.
func localAlreadySent(ctx context.Context, flags *rootFlags, a *app.App, kind, to, newID string) (bool, error) {
	resp, done, err := alreadySentResponse(ctx, a, sendDelegateRequest{Kind: kind, To: to, NewID: newID})
	if err != nil || !done {
		return false, err
	}
	return true, writeDelegatedSendOutput(flags, kind, resp)
}
