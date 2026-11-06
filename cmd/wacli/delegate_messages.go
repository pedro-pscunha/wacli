package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

const (
	forwardKind     = "forward"
	revokeKind      = "revoke"
	deleteForMeKind = "delete_for_me"
)

type forwardResult struct {
	sendMeta
	Forwarded    bool   `json:"forwarded"`
	To           string `json:"to"`
	ID           string `json:"id"`
	Source       string `json:"source"`
	StoreWarning string `json:"store_warning,omitempty"`
}

func (r forwardResult) ProducedID() string { return r.ID }

func (r forwardResult) storeWarning() (string, string) { return r.StoreWarning, r.To }

type revokeResult struct {
	sendMeta
	Revoked bool   `json:"revoked"`
	To      string `json:"to"`
	ID      string `json:"id"`
	Target  string `json:"target"`
}

func (r revokeResult) ProducedID() string { return r.ID }

type deleteForMeResult struct {
	DeletedForMe bool   `json:"deleted_for_me"`
	To           string `json:"to"`
	Target       string `json:"target"`
	DeletedMedia bool   `json:"deleted_media"`
}

func init() {
	registerKind(forwardKind, kindHandler{validate: validateForward, run: func(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
		return forwardMessage(ctx, a, req)
	}})
	registerKind(revokeKind, kindHandler{validate: validateRevoke, run: func(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
		return revokeMessage(ctx, a, req)
	}})
	registerKind(deleteForMeKind, kindHandler{validate: validateDeleteForMe, run: func(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
		return deleteMessageForMe(ctx, a, req)
	}})
}

func validateForward(req sendDelegateRequest) error {
	if err := requireText("--chat", req.Chat); err != nil {
		return err
	}
	if err := validateTargetMessageID("--id", req.ID); err != nil {
		return err
	}
	if err := requireText("--to", req.To); err != nil {
		return err
	}
	if req.Pick < 0 {
		return fmt.Errorf("--pick must be a positive integer, got %d", req.Pick)
	}
	return nil
}

func validateRevoke(req sendDelegateRequest) error {
	if err := requireText("--chat", req.Chat); err != nil {
		return err
	}
	return validateTargetMessageID("--id", req.ID)
}

func validateDeleteForMe(req sendDelegateRequest) error {
	if err := requireText("--chat", req.Chat); err != nil {
		return err
	}
	return validateTargetMessageID("--id", req.ID)
}

func forwardMessage(ctx context.Context, a *app.App, req sendDelegateRequest) (forwardResult, error) {
	source, _, err := loadMessageMutationTarget(ctx, a, req.Chat, req.ID)
	if err != nil {
		return forwardResult{}, err
	}
	if err := validateMessageCanForward(source); err != nil {
		return forwardResult{}, err
	}
	var mediaInfo *store.MediaDownloadInfo
	if strings.TrimSpace(source.MediaType) != "" {
		info, err := a.DB().GetMediaDownloadInfo(source.ChatJID, source.MsgID)
		if err != nil {
			return forwardResult{}, err
		}
		mediaInfo = &info
	}
	toJID, err := resolveRecipient(a, req.To, recipientOptions{pick: req.Pick, asJSON: true})
	if err != nil {
		return forwardResult{}, err
	}
	if mediaInfo != nil && toJID.Server == types.NewsletterServer {
		return forwardResult{}, fmt.Errorf("media forwarding to channels is not supported")
	}
	payload, err := buildForwardedMessage(source, mediaInfo)
	if err != nil {
		return forwardResult{}, err
	}
	if err := ensureLive(ctx, a); err != nil {
		return forwardResult{}, err
	}
	toJID = warmupRecipient(ctx, a.WA(), toJID, os.Stderr)
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return forwardResult{}, err
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().SendProtoMessage(ctx, toJID, payload.Message)
	})
	if err != nil {
		return forwardResult{}, err
	}

	now := time.Now().UTC()
	chatName := a.WA().ResolveChatName(ctx, toJID, "")
	var storeErr error
	if err := a.DB().UpsertChat(toJID.String(), chatKindFromJID(toJID), chatName, now); err != nil {
		storeErr = fmt.Errorf("chat update: %w", err)
	}
	if err := a.DB().UpsertMessage(store.UpsertMessageParams{
		ChatJID:         toJID.String(),
		ChatName:        chatName,
		MsgID:           string(sentID),
		SenderName:      "me",
		Timestamp:       now,
		FromMe:          true,
		Text:            payload.Text,
		DisplayText:     payload.Text,
		MediaType:       payload.MediaType,
		MediaCaption:    payload.MediaCaption,
		Filename:        payload.Filename,
		MimeType:        payload.MimeType,
		DirectPath:      payload.DirectPath,
		MediaKey:        payload.MediaKey,
		FileSHA256:      payload.FileSHA256,
		FileEncSHA256:   payload.FileEncSHA256,
		FileLength:      payload.FileLength,
		IsForwarded:     true,
		ForwardingScore: payload.ForwardingScore,
	}); err != nil {
		storeErr = errors.Join(storeErr, fmt.Errorf("message update: %w", err))
	}
	warnSendStoreFailure(os.Stderr, string(sentID), storeErr)
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))

	res := forwardResult{Forwarded: true, To: toJID.String(), ID: string(sentID), Source: source.MsgID}
	if storeErr != nil {
		res.StoreWarning = storeErr.Error()
	}
	return res, nil
}

func revokeMessage(ctx context.Context, a *app.App, req sendDelegateRequest) (revokeResult, error) {
	var (
		msg     store.Message
		chatJID types.JID
		found   bool
		err     error
	)
	if req.Strict {
		msg, chatJID, err = loadMessageMutationTarget(ctx, a, req.Chat, req.ID)
		found = err == nil
	} else {
		msg, chatJID, found, err = loadMessageRevokeTarget(ctx, a, req.Chat, req.ID)
	}
	if err != nil {
		return revokeResult{}, err
	}
	if found {
		if err := validateMessageCanRevoke(msg); err != nil {
			return revokeResult{}, err
		}
	}
	if err := ensureLive(ctx, a); err != nil {
		return revokeResult{}, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return revokeResult{}, err
	}
	targetID := req.ID
	if found {
		targetID = msg.MsgID
	}
	sentID, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().RevokeMessage(ctx, chatJID, types.MessageID(targetID))
	})
	if err != nil {
		return revokeResult{}, err
	}
	if found {
		if err := a.DB().MarkMessageRevoked(msg.ChatJID, msg.MsgID); err != nil {
			return revokeResult{}, fmt.Errorf("store deleted message state: %w", err)
		}
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	target := req.ID
	if req.Strict && found {
		target = msg.MsgID
	}
	return revokeResult{Revoked: true, To: chatJID.String(), ID: string(sentID), Target: target}, nil
}

func deleteMessageForMe(ctx context.Context, a *app.App, req sendDelegateRequest) (deleteForMeResult, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, req.Chat, req.ID)
	if err != nil {
		return deleteForMeResult{}, err
	}
	if err := validateMessageCanDeleteForMe(msg); err != nil {
		return deleteForMeResult{}, err
	}
	if err := ensureLive(ctx, a); err != nil {
		return deleteForMeResult{}, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return deleteForMeResult{}, err
	}
	info, err := messageInfoForDeleteForMe(msg, chatJID)
	if err != nil {
		return deleteForMeResult{}, err
	}
	// An app-state write: it goes through the App's chat-state gate, not the
	// message send retry, and the daemon's queue does not pace it.
	if err := a.DeleteMessageForMe(ctx, info, req.DeleteMedia); err != nil {
		return deleteForMeResult{}, err
	}
	mediaPaths, err := a.DB().MessageLocalMediaPaths(msg.ChatJID, msg.MsgID)
	if err != nil {
		return deleteForMeResult{}, fmt.Errorf("load local media paths: %w", err)
	}
	deletedMediaCount, deleteMediaErr := deleteLocalMediaPathsIfRequested(req.DeleteMedia, mediaPaths)
	if deleteMediaErr != nil {
		if err := a.DB().MarkMessageDeletedForMePreserveMedia(msg.ChatJID, msg.MsgID); err != nil {
			return deleteForMeResult{}, fmt.Errorf("store deleted-for-me message state: %w", err)
		}
		return deleteForMeResult{}, fmt.Errorf("delete local media: %w", deleteMediaErr)
	}
	if req.DeleteMedia && strings.TrimSpace(msg.LocalPath) != "" {
		if err := a.DB().ClearMessageLocalMedia(msg.ChatJID, msg.MsgID); err != nil {
			return deleteForMeResult{}, fmt.Errorf("clear deleted local media state: %w", err)
		}
	}
	if err := a.DB().MarkMessageDeletedForMe(msg.ChatJID, msg.MsgID, msg.SenderJID, msg.FromMe, time.Now().UTC()); err != nil {
		return deleteForMeResult{}, fmt.Errorf("store deleted-for-me message state: %w", err)
	}
	waitForPostSendRetryReceipts(ctx, millisDuration(req.PostSendWaitMS, 0))
	return deleteForMeResult{DeletedForMe: true, To: chatJID.String(), Target: msg.MsgID, DeletedMedia: deletedMediaCount > 0}, nil
}
