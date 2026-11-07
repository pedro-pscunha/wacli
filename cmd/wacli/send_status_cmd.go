package main

import (
	"context"
	"fmt"
	"github.com/openclaw/wacli/internal/wa"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type statusTextOptions struct {
	BackgroundColor string
	Font            *int32
}

type statusMessageSender interface {
	SendProtoMessage(ctx context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error)
}

func newSendStatusCmd(flags *rootFlags) *cobra.Command {
	var message string
	var backgroundColor string
	var font int32
	var fontSet bool
	var filePath string
	var mimeOverride string
	var postSendWait = postSendRetryReceiptWait

	var newMsgID string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Send a status broadcast",
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath = strings.TrimSpace(filePath)
			if strings.TrimSpace(message) == "" && filePath == "" {
				return fmt.Errorf("--message or --file is required")
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			if filePath != "" {
				if err := checkOutboundMediaPath(filePath); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("font") {
				fontSet = true
			}
			var fontPtr *int32
			if fontSet {
				fontPtr = &font
			}
			if err := validateNewMessageID(newMsgID); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			ctx = wa.WithMessageID(ctx, newMsgID)
			delegateFile := filePath
			if filePath != "" {
				// The daemon resolves a relative path against its own directory: send it an absolute one.
				abs, absErr := filepath.Abs(filePath)
				if absErr != nil {
					return absErr
				}
				delegateFile = abs
			}
			req := sendDelegateRequest{Kind: statusPostKind, Message: message, File: delegateFile, MIME: mimeOverride,
				BackgroundColor: backgroundColor, Font: fontPtr, NewID: newMsgID, PostSendWaitMS: durationMillis(postSendWait)}
			res, err := callKind[actionAck](ctx, flags, req)
			if err != nil {
				return err
			}
			if flags.asJSON {
				body := map[string]any{"sent": true, "to": res.Chat, "id": res.ID}
				if res.Media != "" {
					body["media"] = res.Media
				}
				if res.MIMEType != "" {
					body["mime_type"] = res.MIMEType
				}
				if res.Audience != nil {
					body["audience"] = res.Audience
				}
				addSendMetaFields(body, res.sendMeta)
				return out.WriteJSON(os.Stdout, body)
			}
			fmt.Fprintf(os.Stdout, "Sent status (id %s)\n", res.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "status text or media caption")
	cmd.Flags().StringVar(&newMsgID, "id", "", "caller-chosen WhatsApp message ID for this send (8-64 letters or digits; default: random)")
	cmd.Flags().StringVar(&filePath, "file", "", "media file to send as a status update")
	cmd.Flags().StringVar(&mimeOverride, "mime", "", "override detected MIME type for --file")
	cmd.Flags().StringVar(&backgroundColor, "background-color", "", "text status background color (#RRGGBB or #AARRGGBB)")
	cmd.Flags().Int32Var(&font, "font", 0, "WhatsApp text status font number")
	cmd.Flags().DurationVar(&postSendWait, "post-send-wait", postSendRetryReceiptWait, "keep the connection alive after send so retry receipts can be handled (0 disables)")
	return cmd
}

func buildStatusTextMessage(text string, opts statusTextOptions) (*waProto.Message, error) {
	ext := &waProto.ExtendedTextMessage{Text: proto.String(text)}
	if strings.TrimSpace(opts.BackgroundColor) != "" {
		color, err := parseStatusBackgroundColor(opts.BackgroundColor)
		if err != nil {
			return nil, err
		}
		ext.BackgroundArgb = proto.Uint32(color)
	}
	if opts.Font != nil {
		ext.Font = waProto.ExtendedTextMessage_FontType(*opts.Font).Enum()
	}
	return &waProto.Message{ExtendedTextMessage: ext}, nil
}

func parseStatusBackgroundColor(s string) (uint32, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	if len(s) == 6 {
		s = "ff" + s
	}
	if len(s) != 8 {
		return 0, fmt.Errorf("--background-color must be #RRGGBB or #AARRGGBB")
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid --background-color: %w", err)
	}
	return uint32(v), nil
}

func sendStatusTextMessage(ctx context.Context, sender statusMessageSender, msg *waProto.Message) (types.MessageID, error) {
	return sender.SendProtoMessage(ctx, types.StatusBroadcastJID, msg)
}
