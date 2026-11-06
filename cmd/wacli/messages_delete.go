package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

func newMessagesDeleteCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var forMe bool
	var deleteMedia bool
	var newMsgID string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a message for everyone or for you",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			if deleteMedia && !forMe {
				return fmt.Errorf("--delete-media requires --for-me")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			if err := validateNewMessageID(newMsgID); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			ctx = wa.WithMessageID(ctx, newMsgID)
			if forMe {
				res, err := callKind[deleteForMeResult](ctx, flags, sendDelegateRequest{
					Kind: deleteForMeKind, Chat: chat, ID: id, DeleteMedia: deleteMedia,
					PostSendWaitMS: durationMillis(postSendWait),
				})
				if err != nil {
					return err
				}
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{
						"deleted_for_me": true,
						"to":             res.To,
						"target":         res.Target,
						"deleted_media":  res.DeletedMedia,
					})
				}
				fmt.Fprintf(os.Stdout, "Deleted message %s for me in %s\n", res.Target, res.To)
				return nil
			}
			res, err := callKind[revokeResult](ctx, flags, sendDelegateRequest{
				Kind: revokeKind, Chat: chat, ID: id, NewID: newMsgID, Strict: true,
				PostSendWaitMS: durationMillis(postSendWait),
			})
			if err != nil {
				return err
			}
			return writeRevokeOutput(flags, res, "Deleted")
		},
	}
	cmd.Flags().StringVar(&newMsgID, "new-id", "", "caller-chosen WhatsApp message ID for the revoke message (8-64 letters or digits; default: random)")
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID, phone number, or contact/group/chat name")
	cmd.Flags().StringVar(&id, "id", "", "message ID to delete")
	cmd.Flags().BoolVar(&forMe, "for-me", false, "delete the message only for this WhatsApp account")
	cmd.Flags().BoolVar(&deleteMedia, "delete-media", false, "also remove local media when used with --for-me")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after delete so retry receipts can be handled (0 disables)")
	return cmd
}

func newMessagesRevokeCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var newMsgID string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Delete one of your sent messages for everyone",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}

			if err := validateNewMessageID(newMsgID); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			ctx = wa.WithMessageID(ctx, newMsgID)
			res, err := callKind[revokeResult](ctx, flags, sendDelegateRequest{
				Kind: revokeKind, Chat: chat, ID: id, NewID: newMsgID,
				PostSendWaitMS: durationMillis(postSendWait),
			})
			if err != nil {
				return err
			}
			return writeRevokeOutput(flags, res, "Revoked")
		},
	}
	cmd.Flags().StringVar(&newMsgID, "new-id", "", "caller-chosen WhatsApp message ID for the revoke message (8-64 letters or digits; default: random)")
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID to revoke")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after revoke so retry receipts can be handled (0 disables)")
	return cmd
}

func writeRevokeOutput(flags *rootFlags, res revokeResult, verb string) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"revoked": true,
			"to":      res.To,
			"id":      res.ID,
			"target":  res.Target,
		})
	}
	fmt.Fprintf(os.Stdout, "%s message %s in %s (id %s)\n", verb, res.Target, res.To, res.ID)
	return nil
}

func loadMessageRevokeTarget(ctx context.Context, a *app.App, chat, id string) (store.Message, types.JID, bool, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, chat, id)
	if err == nil {
		return msg, chatJID, true, nil
	}
	if !isNoRows(err) {
		return store.Message{}, types.JID{}, false, err
	}
	chatJID, parseErr := wa.ParseUserOrJID(chat)
	if parseErr != nil {
		return store.Message{}, types.JID{}, false, err
	}
	return store.Message{}, chatJID, false, nil
}

func deleteLocalMediaPathsIfRequested(deleteMedia bool, paths []string) (int, error) {
	if !deleteMedia {
		return 0, nil
	}
	deleted := 0
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func validateMessageCanRevoke(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is already deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	if !msg.FromMe {
		return fmt.Errorf("message %s was not sent by me", msg.MsgID)
	}
	return nil
}

func validateMessageCanDeleteForMe(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is already deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	return nil
}

func messageInfoForDeleteForMe(msg store.Message, chat types.JID) (types.MessageInfo, error) {
	sender := types.EmptyJID
	if strings.TrimSpace(msg.SenderJID) != "" {
		parsed, err := types.ParseJID(msg.SenderJID)
		if err != nil {
			return types.MessageInfo{}, fmt.Errorf("stored sender JID is invalid: %w", err)
		}
		sender = parsed
	} else if !msg.FromMe && chat.Server == types.DefaultUserServer {
		sender = chat
	}
	if !msg.FromMe && chat.Server == types.GroupServer && sender.IsEmpty() {
		return types.MessageInfo{}, fmt.Errorf("stored sender JID is required to delete a group message for me")
	}
	return types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   sender,
			IsFromMe: msg.FromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        types.MessageID(msg.MsgID),
		Timestamp: msg.Timestamp,
	}, nil
}
