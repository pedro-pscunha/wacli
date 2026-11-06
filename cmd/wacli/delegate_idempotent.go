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
// goes on the wire it looks the id up among this account's sent messages. A
// hit in the target chat answers "already sent" with that id and sends nothing.
// A hit in another chat is an error: the id is spent. done is true when the
// request is answered here.
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
	target := strings.TrimSpace(req.To)
	chats, ferr := messageChatJIDFilter(ctx, a, target)
	if target == "" || ferr != nil {
		// The target cannot be resolved here; the id is stored, so do not resend.
		chats = []string{foundChat}
	}
	inTarget := false
	for _, c := range chats {
		if c == foundChat {
			inTarget = true
		}
	}
	if !inTarget {
		return sendDelegateResponse{}, false, fmt.Errorf("message id %s was already used for a message in %s, not %s; choose another id", req.NewID, foundChat, target)
	}
	resp := sendDelegateResponse{OK: true, Sent: true, AlreadySent: true, To: foundChat, ID: req.NewID}
	if req.Kind == forwardKind {
		raw, _ := json.Marshal(forwardResult{Forwarded: true, To: foundChat, ID: req.NewID, Source: req.ID})
		resp.Data = raw
	}
	return resp, true, nil
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
