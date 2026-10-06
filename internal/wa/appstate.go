package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
)

func (c *Client) SendAppState(ctx context.Context, patch appstate.PatchInfo) error {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return fmt.Errorf("not connected")
	}
	return cli.SendAppState(ctx, patch)
}

func (c *Client) sendAppStateWithBoundary(ctx context.Context, patch appstate.PatchInfo, beforeApply func()) ([]any, error) {
	// Reserve wacli persistence before whatsmeow can advance app state. The
	// durable recovery intent remains after return because canonical sends
	// dispatch their resulting events asynchronously.
	beforeApply()
	return nil, c.SendAppState(ctx, patch)
}

func (c *Client) ArchiveChat(ctx context.Context, target types.JID, archive bool, lastMsgTS time.Time, lastMsgKey *waCommon.MessageKey, beforeApply func()) ([]any, error) {
	return c.sendAppStateWithBoundary(ctx, appstate.BuildArchive(target, archive, lastMsgTS, lastMsgKey), beforeApply)
}

func (c *Client) PinChat(ctx context.Context, target types.JID, pin bool, beforeApply func()) ([]any, error) {
	return c.sendAppStateWithBoundary(ctx, appstate.BuildPin(target, pin), beforeApply)
}

func (c *Client) MuteChat(ctx context.Context, target types.JID, mute bool, duration time.Duration, beforeApply func()) ([]any, error) {
	return c.sendAppStateWithBoundary(ctx, appstate.BuildMute(target, mute, duration), beforeApply)
}

func (c *Client) MarkChatAsRead(ctx context.Context, target types.JID, read bool, lastMsgTS time.Time, lastMsgKey *waCommon.MessageKey, beforeApply func()) ([]any, error) {
	return c.sendAppStateWithBoundary(ctx, appstate.BuildMarkChatAsRead(target, read, lastMsgTS, lastMsgKey), beforeApply)
}

// EditLabel creates, renames, recolors, or deletes a label. Labels live in the
// regular collection, not regular_low or regular_high like the chat flags.
func (c *Client) EditLabel(ctx context.Context, labelID, name string, color int32, deleted bool) error {
	return c.SendAppState(ctx, appstate.BuildLabelEdit(labelID, name, color, deleted))
}

// LabelChat attaches or detaches one label on one chat.
func (c *Client) LabelChat(ctx context.Context, target types.JID, labelID string, labeled bool) error {
	return c.SendAppState(ctx, appstate.BuildLabelChat(target, labelID, labeled))
}
