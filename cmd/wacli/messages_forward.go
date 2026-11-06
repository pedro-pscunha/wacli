package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func newMessagesForwardCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var id string
	var to string
	var pick int
	var newMsgID string
	postSendWait := postSendRetryReceiptWait

	cmd := &cobra.Command{
		Use:   "forward",
		Short: "Forward a stored message",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" || strings.TrimSpace(to) == "" {
				return fmt.Errorf("--chat, --id, and --to are required")
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

			res, err := callKind[forwardResult](ctx, flags, sendDelegateRequest{
				Kind: forwardKind, Chat: chat, ID: id, To: to, Pick: pick, NewID: newMsgID,
				PostSendWaitMS: durationMillis(postSendWait),
			})
			if err != nil {
				return err
			}
			if res.StoreWarning != "" {
				warnSendStoreFailureMsg(os.Stderr, res.ID, res.StoreWarning)
			}
			if flags.asJSON {
				body := map[string]any{"forwarded": true, "to": res.To, "id": res.ID, "source": res.Source}
				addSendMetaFields(body, res.sendMeta)
				if res.StoreWarning != "" {
					body["store_warning"] = res.StoreWarning
				}
				return out.WriteJSON(os.Stdout, body)
			}
			fmt.Fprintf(os.Stdout, "Forwarded message %s to %s (id %s)\n", res.Source, res.To, res.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&newMsgID, "new-id", "", "caller-chosen WhatsApp message ID for the forwarded message (8-64 letters or digits; default: random)")
	cmd.Flags().StringVar(&chat, "chat", "", "source chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "source message ID to forward")
	cmd.Flags().StringVar(&to, "to", "", "recipient JID, phone number, or contact/group/chat name")
	cmd.Flags().IntVar(&pick, "pick", 0, "when --to is ambiguous, pick the Nth match (1-indexed)")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after forward so retry receipts can be handled (0 disables)")
	return cmd
}

func validateMessageCanForward(msg store.Message) error {
	if msg.Revoked {
		return fmt.Errorf("message %s is deleted", msg.MsgID)
	}
	if msg.DeletedForMe {
		return fmt.Errorf("message %s was deleted for me", msg.MsgID)
	}
	if strings.TrimSpace(msg.ReactionToID) != "" {
		return fmt.Errorf("reaction messages cannot be forwarded")
	}
	mediaType := strings.ToLower(strings.TrimSpace(msg.MediaType))
	if mediaType != "" && !isStoredMediaType(mediaType) {
		return fmt.Errorf("%s messages cannot be forwarded", mediaType)
	}
	if mediaType == "" && strings.TrimSpace(messageForwardText(msg)) == "" {
		return fmt.Errorf("only text messages can be forwarded")
	}
	return nil
}

func messageForwardText(msg store.Message) string {
	if strings.TrimSpace(msg.Text) != "" {
		return msg.Text
	}
	if strings.TrimSpace(msg.DisplayText) != "" {
		return msg.DisplayText
	}
	return ""
}

type forwardedMessagePayload struct {
	Message         *waProto.Message
	Text            string
	MediaType       string
	MediaCaption    string
	Filename        string
	MimeType        string
	DirectPath      string
	MediaKey        []byte
	FileSHA256      []byte
	FileEncSHA256   []byte
	FileLength      uint64
	ForwardingScore uint32
}

func buildForwardedMessage(msg store.Message, mediaInfo *store.MediaDownloadInfo) (forwardedMessagePayload, error) {
	forwardingScore := msg.ForwardingScore + 1
	mediaType := strings.ToLower(strings.TrimSpace(msg.MediaType))
	if mediaType == "" {
		text := messageForwardText(msg)
		return forwardedMessagePayload{
			Message:         buildForwardedTextMessage(text, forwardingScore),
			Text:            text,
			ForwardingScore: forwardingScore,
		}, nil
	}
	if mediaInfo == nil {
		return forwardedMessagePayload{}, fmt.Errorf("message has no media metadata")
	}
	if err := validateStoredMediaInfo(*mediaInfo); err != nil {
		return forwardedMessagePayload{}, err
	}

	payload := forwardedMessagePayload{
		Text:            msg.MediaCaption,
		MediaType:       mediaType,
		MediaCaption:    msg.MediaCaption,
		Filename:        mediaInfo.Filename,
		MimeType:        mediaInfo.MimeType,
		DirectPath:      mediaInfo.DirectPath,
		MediaKey:        append([]byte(nil), mediaInfo.MediaKey...),
		FileSHA256:      append([]byte(nil), mediaInfo.FileSHA256...),
		FileEncSHA256:   append([]byte(nil), mediaInfo.FileEncSHA256...),
		FileLength:      mediaInfo.FileLength,
		ForwardingScore: forwardingScore,
	}
	ctx := forwardedContextInfo(forwardingScore)
	message, err := buildStoredMediaMessage(mediaType, msg.MediaCaption, *mediaInfo, ctx)
	if err != nil {
		return forwardedMessagePayload{}, err
	}
	payload.Message = message
	if mediaType == "document" && strings.TrimSpace(payload.Filename) == "" {
		payload.Filename = "document"
	}
	return payload, nil
}

func buildForwardedTextMessage(text string, forwardingScore uint32) *waProto.Message {
	return &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: forwardedContextInfo(forwardingScore),
		},
	}
}

func forwardedContextInfo(forwardingScore uint32) *waProto.ContextInfo {
	if forwardingScore == 0 {
		forwardingScore = 1
	}
	return &waProto.ContextInfo{
		IsForwarded:     proto.Bool(true),
		ForwardingScore: proto.Uint32(forwardingScore),
	}
}
