package wa

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The methods in this file are the account-level actions that need the live connection and nothing else. Each
// one is a thin call into whatsmeow behind the same "not connected" check as the rest of Client.

func (c *Client) live() (*whatsmeow.Client, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || !cli.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}
	return cli, nil
}

// StarMessage stars or unstars one message (an app-state write in the regular_high collection).
func (c *Client) StarMessage(ctx context.Context, info types.MessageInfo, starred bool) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	if info.Chat.IsEmpty() || info.ID == "" {
		return fmt.Errorf("message chat and ID are required")
	}
	sender := info.Sender
	if sender.IsEmpty() {
		sender = info.Chat
	}
	return cli.SendAppState(ctx, appstate.BuildStar(info.Chat, sender, info.ID, info.IsFromMe, starred))
}

// PinInChat pins or unpins a message inside a chat for everyone. It is a message on the wire, and the server
// cannot read it: success means "sent", not "applied". A recipient that cannot apply the pin drops it silently.
func (c *Client) PinInChat(ctx context.Context, chat types.JID, sender types.JID, fromMe bool, id types.MessageID, pin bool, duration time.Duration) (types.MessageID, error) {
	cli, err := c.live()
	if err != nil {
		return "", err
	}
	if fromMe {
		sender = types.EmptyJID // BuildMessageKey reads an empty sender as the account's own message
	}
	kind := waE2E.PinInChatMessage_PIN_FOR_ALL
	if !pin {
		kind = waE2E.PinInChatMessage_UNPIN_FOR_ALL
	}
	if !pin {
		duration = 0
	}
	msg := &waE2E.Message{
		PinInChatMessage: &waE2E.PinInChatMessage{
			Key:               cli.BuildMessageKey(chat, sender, id),
			Type:              kind.Enum(),
			SenderTimestampMS: proto.Int64(time.Now().UnixMilli()),
		},
		// Like WhatsApp Web and Baileys: the duration only, 0 on unpin, no expiry type.
		MessageContextInfo: &waE2E.MessageContextInfo{MessageAddOnDurationInSecs: proto.Uint32(uint32(duration.Seconds()))},
	}
	markDispatched(ctx)
	resp, err := cli.SendMessage(ctx, chat, msg, sendExtras(ctx, "")...)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// StatusAudience says who a status post reaches, with the rule whatsmeow uses to build the recipient list:
// a whitelist is that list; contacts and blacklist are the address-book contacts that have a full name, minus
// the blacklist. It sends one IQ (the status privacy) and reads the session store.
func (c *Client) StatusAudience(ctx context.Context) (types.StatusPrivacyType, int, error) {
	cli, err := c.live()
	if err != nil {
		return "", 0, err
	}
	opts, err := cli.GetStatusPrivacy(ctx)
	if err != nil {
		return "", 0, err
	}
	if len(opts) == 0 {
		return types.StatusPrivacyTypeContacts, 0, fmt.Errorf("no status privacy in the answer")
	}
	p := opts[0]
	if p.Type == types.StatusPrivacyTypeWhitelist {
		return p.Type, len(p.List), nil
	}
	if cli.Store == nil || cli.Store.Contacts == nil {
		return p.Type, 0, fmt.Errorf("contacts store not available")
	}
	contacts, err := cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return p.Type, 0, err
	}
	skip := map[types.JID]struct{}{}
	if p.Type == types.StatusPrivacyTypeBlacklist {
		for _, j := range p.List {
			skip[j] = struct{}{}
		}
	}
	n := 0
	for jid, ci := range contacts {
		if _, no := skip[jid]; no || len(ci.FullName) == 0 {
			continue
		}
		n++
	}
	return p.Type, n, nil
}

// UpdateBlocklist blocks or unblocks one contact and returns the new list.
func (c *Client) UpdateBlocklist(ctx context.Context, jid types.JID, block bool) (*types.Blocklist, error) {
	cli, err := c.live()
	if err != nil {
		return nil, err
	}
	action := events.BlocklistChangeActionUnblock
	if block {
		action = events.BlocklistChangeActionBlock
	}
	return cli.UpdateBlocklist(ctx, jid, action, "")
}

func (c *Client) GetBlocklist(ctx context.Context) (*types.Blocklist, error) {
	cli, err := c.live()
	if err != nil {
		return nil, err
	}
	return cli.GetBlocklist(ctx, "")
}

// SetGroupPhoto sets the group picture (JPEG bytes); nil removes it.
func (c *Client) SetGroupPhoto(ctx context.Context, jid types.JID, avatar []byte) (string, error) {
	cli, err := c.live()
	if err != nil {
		return "", err
	}
	return cli.SetGroupPhoto(ctx, jid, avatar)
}

func (c *Client) SetGroupJoinApprovalMode(ctx context.Context, jid types.JID, on bool) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	return cli.SetGroupJoinApprovalMode(ctx, jid, on)
}

// SetGroupMemberAddMode sets who can add members: every member, or admins only.
func (c *Client) SetGroupMemberAddMode(ctx context.Context, jid types.JID, allMembers bool) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	mode := types.GroupMemberAddModeAdmin
	if allMembers {
		mode = types.GroupMemberAddModeAllMember
	}
	return cli.SetGroupMemberAddMode(ctx, jid, mode)
}

func (c *Client) SetDisappearingTimer(ctx context.Context, chat types.JID, timer time.Duration) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	markDispatched(ctx)
	return cli.SetDisappearingTimer(ctx, chat, timer, time.Time{})
}

func (c *Client) SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	return cli.SetDefaultDisappearingTimer(ctx, timer)
}

func (c *Client) NewsletterToggleMute(ctx context.Context, jid types.JID, mute bool) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	return cli.NewsletterToggleMute(ctx, jid, mute)
}

func (c *Client) CreateNewsletter(ctx context.Context, name, description string) (*types.NewsletterMetadata, error) {
	cli, err := c.live()
	if err != nil {
		return nil, err
	}
	return cli.CreateNewsletter(ctx, whatsmeow.CreateNewsletterParams{Name: name, Description: description})
}

// DeleteChat deletes a chat on the phone and on every linked device (an app-state write).
func (c *Client) DeleteChat(ctx context.Context, chat types.JID, lastMsgTS time.Time, deleteMedia bool) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	return cli.SendAppState(ctx, appstate.BuildDeleteChat(chat, lastMsgTS, nil, deleteMedia))
}

func (c *Client) GetPrivacySettings(ctx context.Context) (types.PrivacySettings, error) {
	cli, err := c.live()
	if err != nil {
		return types.PrivacySettings{}, err
	}
	s, err := cli.TryFetchPrivacySettings(ctx, true)
	if err != nil || s == nil {
		return types.PrivacySettings{}, err
	}
	return *s, nil
}

func (c *Client) SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error) {
	cli, err := c.live()
	if err != nil {
		return types.PrivacySettings{}, err
	}
	return cli.SetPrivacySetting(ctx, name, value)
}

func (c *Client) RejectCall(ctx context.Context, from types.JID, callID string) error {
	cli, err := c.live()
	if err != nil {
		return err
	}
	return cli.RejectCall(ctx, from, callID)
}
