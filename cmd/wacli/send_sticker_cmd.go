package main

import (
	"context"
	"fmt"
	"github.com/openclaw/wacli/internal/wa"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func newSendStickerCmd(flags *rootFlags) *cobra.Command {
	var to string
	var pick int
	var filePath string
	var replyTo string
	var replyToSender string
	postSendWait := postSendRetryReceiptWait

	var newMsgID string
	cmd := &cobra.Command{
		Use:   "sticker",
		Short: "Send a sticker (WebP image)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" || filePath == "" {
				return fmt.Errorf("--to and --file are required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if err := checkOutboundMediaPath(filePath); err != nil {
				return err
			}

			if err := validateNewMessageID(newMsgID); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			ctx = wa.WithMessageID(ctx, newMsgID)

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				delegateFile := filePath
				if abs, absErr := filepath.Abs(filePath); absErr == nil {
					delegateFile = abs
				}
				resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{
					Kind:           "sticker",
					NewID:          newMsgID,
					To:             to,
					Pick:           pick,
					File:           delegateFile,
					ReplyTo:        replyTo,
					ReplyToSender:  replyToSender,
					PostSendWaitMS: durationMillis(postSendWait),
				})
				if delegated {
					if delegateErr != nil {
						return delegateErr
					}
					return writeDelegatedSendOutput(flags, "sticker", resp)
				}
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			ctx, finishID, idDone, idErr := localIDGuard(ctx, flags, a, "sticker", to, "", newMsgID)
			if idDone {
				return idErr
			}
			var sendErr error
			var sendStoreWarning string
			defer func() { finishID(sendErr, sendStoreWarning) }()

			toJID, err := resolveRecipient(a, to, recipientOptions{pick: pick, asJSON: flags.asJSON})
			if err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			toJID = warmupRecipient(ctx, a.WA(), toJID, os.Stderr)
			if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
				return err
			}

			res, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (sendFileOutcome, error) {
				return sendSticker(ctx, a, toJID, filePath, sendStickerOptions{
					replyTo:       replyTo,
					replyToSender: replyToSender,
				})
			})
			sendErr = err
			if err == nil && res.storeWarning != nil {
				sendStoreWarning = res.storeWarning.Error()
			}
			if err != nil {
				return err
			}
			warnSendStoreFailure(os.Stderr, res.id, res.storeWarning)

			waitForPostSendRetryReceipts(ctx, postSendWait)

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, addStoreWarning(map[string]any{
					"sent": true,
					"to":   toJID.String(),
					"id":   res.id,
					"file": res.meta,
				}, res.storeWarning))
			}
			fmt.Fprintf(os.Stdout, "Sent sticker to %s (id %s)\n", toJID.String(), res.id)
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "recipient JID, phone number, or contact/group/chat name")
	cmd.Flags().StringVar(&newMsgID, "id", "", "caller-chosen WhatsApp message ID for this send (8-64 letters or digits; default: random)")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --to is ambiguous, pick the Nth match (1-indexed)")
	cmd.Flags().StringVar(&filePath, "file", "", "path to WebP sticker file")
	cmd.Flags().StringVar(&replyTo, "reply-to", "", "message ID to quote/reply to")
	cmd.Flags().StringVar(&replyToSender, "reply-to-sender", "", "sender JID of the quoted message (required for unsynced group replies)")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}
