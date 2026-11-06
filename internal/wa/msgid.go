package wa

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"

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

// SendPhase records whether a send reached the wire. SendMessage builds the
// frame, encrypts and writes it in one call, so the mark is set right before
// that call: an error from anything earlier (upload, recipient lookup, a
// missing connection) is definite, and an error after the mark may have come
// from a frame the server already has.
type SendPhase struct {
	dispatched atomic.Bool
}

// Dispatched reports whether the send reached the point of no return.
func (p *SendPhase) Dispatched() bool { return p != nil && p.dispatched.Load() }

type sendPhaseKey struct{}

// WithSendPhase returns a context carrying a fresh SendPhase, and the phase.
func WithSendPhase(ctx context.Context) (context.Context, *SendPhase) {
	p := &SendPhase{}
	return context.WithValue(ctx, sendPhaseKey{}, p), p
}

// SendPhaseFrom returns the phase carried by ctx, or nil. A client implementation
// marks it right before it hands a message to the wire; fakes do the same.
func SendPhaseFrom(ctx context.Context) *SendPhase {
	p, _ := ctx.Value(sendPhaseKey{}).(*SendPhase)
	return p
}

// MarkDispatched records that the send reached the wire. It is safe on a nil
// phase.
func (p *SendPhase) MarkDispatched() {
	if p != nil {
		p.dispatched.Store(true)
	}
}

// markDispatched is called by the client just before it hands a message to
// whatsmeow.
func markDispatched(ctx context.Context) { SendPhaseFrom(ctx).MarkDispatched() }

// ErrorAfterDispatchIsUnknown reports whether err, raised after the dispatch
// mark, leaves it open that the server received the message. The server's own
// error reply is definite: it answered. Everything else (a dropped websocket, a
// timeout waiting for the ack, a cancelled context) is not.
func ErrorAfterDispatchIsUnknown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, whatsmeow.ErrServerReturnedError) {
		return false
	}
	var iqErr *whatsmeow.IQError
	if errors.As(err, &iqErr) {
		return false
	}
	return true
}
