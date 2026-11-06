package wa

import (
	"context"
	"fmt"
	"regexp"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type messageIDKey struct{}

var messageIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// ValidateMessageID checks a caller-chosen WhatsApp message ID: 8 to 64
// letters or digits (whatsmeow's own IDs are "3EB0" plus 16 hex characters).
func ValidateMessageID(id string) error {
	if !messageIDPattern.MatchString(id) {
		return fmt.Errorf("invalid message id %q: use 8-64 letters or digits", id)
	}
	return nil
}

// WithMessageID returns a context that makes the next send use id as its
// WhatsApp message ID. An empty id leaves the default random ID.
func WithMessageID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, messageIDKey{}, types.MessageID(id))
}

// MessageIDFromContext returns the ID set by WithMessageID, or "".
func MessageIDFromContext(ctx context.Context) types.MessageID {
	id, _ := ctx.Value(messageIDKey{}).(types.MessageID)
	return id
}

// sendExtras builds the single SendRequestExtra whatsmeow accepts. It returns
// nil when neither a caller-chosen ID nor a media handle is set.
func sendExtras(ctx context.Context, mediaHandle string) []whatsmeow.SendRequestExtra {
	id := MessageIDFromContext(ctx)
	if id == "" && mediaHandle == "" {
		return nil
	}
	return []whatsmeow.SendRequestExtra{{ID: id, MediaHandle: mediaHandle}}
}
