package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

// Account actions a person can do in the WhatsApp app and that had no daemon kind. Every kind here runs on the
// sync daemon's connection (ensureLive never dials in the daemon). Four kinds put a message on the wire and so
// take the paced send slot: message_pin, chat_disappearing, status_post and status_delete. The others are IQs
// or app-state writes and skip the queue. TestActionKindsPacing pins both sets.
const (
	messageStarKind       = "message_star"
	messagePinKind        = "message_pin"
	contactBlockKind      = "contact_block"
	blocklistKind         = "blocklist"
	groupPhotoKind        = "group_photo"
	chatDisappearingKind  = "chat_disappearing"
	disappearingDefaultKd = "disappearing_default"
	statusPostKind        = "status_post"
	statusDeleteKind      = "status_delete"
	channelMuteKind       = "channel_mute"
	channelCreateKind     = "channel_create"
	chatDeleteKind        = "chat_delete"
	privacyGetKind        = "privacy_get"
	privacySetKind        = "privacy_set"
	callRejectKind        = "call_reject"
)

// pacedActionKinds are the kinds of this file that go through the send queue.
var pacedActionKinds = []string{messagePinKind, chatDisappearingKind, statusPostKind, statusDeleteKind}

type actionAck struct {
	sendMeta
	Chat      string   `json:"chat,omitempty"`
	JID       string   `json:"jid,omitempty"`
	Target    string   `json:"target,omitempty"`
	ID        string   `json:"id,omitempty"`
	Starred   *bool    `json:"starred,omitempty"`
	Pinned    *bool    `json:"pinned,omitempty"`
	Blocked   *bool    `json:"blocked,omitempty"`
	Muted     *bool    `json:"muted,omitempty"`
	Deleted   *bool    `json:"deleted,omitempty"`
	Rejected  *bool    `json:"rejected,omitempty"`
	Duration  string   `json:"duration,omitempty"`
	Name      string   `json:"name,omitempty"`
	PictureID string   `json:"picture_id,omitempty"`
	JIDs      []string `json:"jids,omitempty"`
	Settings  any      `json:"settings,omitempty"`
}

// sentAck is the answer of a kind that put a message on the wire: ProducedID makes the response carry sent and
// the new message id, which the id-mismatch check keys on. The kinds without a message answer with actionAck.
type sentAck struct{ actionAck }

func (r sentAck) ProducedID() string { return r.ID }

func (r sentAck) wasAlreadySent() bool { return r.AlreadySent }

func boolPtr(b bool) *bool { return &b }

func init() {
	registerKind(messageStarKind, kindHandler{skipsQueue: true, validate: validateMessageToggle, run: runMessageStar})
	registerKind(messagePinKind, kindHandler{validate: validateMessagePin, run: runMessagePin})
	registerKind(contactBlockKind, kindHandler{skipsQueue: true, validate: validateContactBlock, run: runContactBlock})
	registerKind(blocklistKind, kindHandler{skipsQueue: true, validate: func(sendDelegateRequest) error { return nil }, run: runBlocklist})
	registerKind(groupPhotoKind, kindHandler{skipsQueue: true, validate: validateGroupPhoto, run: runGroupPhoto})
	registerKind(chatDisappearingKind, kindHandler{validate: validateChatDisappearing, run: runChatDisappearing})
	registerKind(disappearingDefaultKd, kindHandler{skipsQueue: true, validate: func(r sendDelegateRequest) error {
		_, err := parseDisappearingTimer(r.EphemeralDuration)
		return err
	}, run: runDisappearingDefault})
	registerKind(statusPostKind, kindHandler{validate: validateStatusPost, run: runStatusPost})
	registerKind(statusDeleteKind, kindHandler{validate: func(r sendDelegateRequest) error {
		return validateTargetMessageID("--id", r.ID)
	}, run: runStatusDelete})
	registerKind(channelMuteKind, kindHandler{skipsQueue: true, validate: validateChannelMute, run: runChannelMute})
	registerKind(channelCreateKind, kindHandler{skipsQueue: true, validate: validateChannelCreate, run: runChannelCreate})
	registerKind(chatDeleteKind, kindHandler{skipsQueue: true, validate: validateChatDelete, run: runChatDelete})
	registerKind(privacyGetKind, kindHandler{skipsQueue: true, validate: func(sendDelegateRequest) error { return nil }, run: runPrivacyGet})
	registerKind(privacySetKind, kindHandler{skipsQueue: true, validate: validatePrivacySet, run: runPrivacySet})
	registerKind(callRejectKind, kindHandler{skipsQueue: true, validate: validateCallReject, run: runCallReject})
}

// ---- messages: star, pin ----

func validateMessageToggle(r sendDelegateRequest) error {
	if err := requireText("--chat", r.Chat); err != nil {
		return err
	}
	if err := validateTargetMessageID("--id", r.ID); err != nil {
		return err
	}
	if r.Enable == nil {
		return fmt.Errorf("enable is required (true or false)")
	}
	return nil
}

func runMessageStar(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, r.Chat, r.ID)
	if err != nil {
		return nil, err
	}
	info, err := messageInfoForDeleteForMe(msg, chatJID)
	if err != nil {
		return nil, err
	}
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := a.StarMessage(ctx, info, *r.Enable); err != nil {
		return nil, fmt.Errorf("star message: %w", err)
	}
	return actionAck{Chat: chatJID.String(), Target: msg.MsgID, Starred: boolPtr(*r.Enable)}, nil
}

func validateMessagePin(r sendDelegateRequest) error {
	if err := validateMessageToggle(r); err != nil {
		return err
	}
	if *r.Enable {
		if _, err := parsePinDuration(r.EphemeralDuration); err != nil {
			return err
		}
	} else if strings.TrimSpace(r.EphemeralDuration) != "" {
		return fmt.Errorf("duration only applies to a pin")
	}
	return nil
}

func parsePinDuration(s string) (time.Duration, error) {
	switch strings.TrimSpace(s) {
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "30d":
		return 30 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unsupported pin duration %q (24h|7d|30d)", s)
}

func runMessagePin(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	msg, chatJID, err := loadMessageMutationTarget(ctx, a, r.Chat, r.ID)
	if err != nil {
		return nil, err
	}
	info, err := messageInfoForDeleteForMe(msg, chatJID)
	if err != nil {
		return nil, err
	}
	var dur time.Duration
	if *r.Enable {
		dur, _ = parsePinDuration(r.EphemeralDuration)
	}
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return nil, err
	}
	id, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().PinInChat(ctx, chatJID, info.Sender, msg.FromMe, types.MessageID(msg.MsgID), *r.Enable, dur)
	})
	if err != nil {
		return nil, err
	}
	return sentAck{actionAck{Chat: chatJID.String(), Target: msg.MsgID, ID: string(id), Pinned: boolPtr(*r.Enable)}}, nil
}

// ---- contacts: block ----

func parseUserJID(raw string) (types.JID, error) {
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		return types.JID{}, err
	}
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return types.JID{}, fmt.Errorf("expected a phone number or a user JID, got %s", jid.String())
	}
	return canonicalCLIJID(jid), nil
}

func validateContactBlock(r sendDelegateRequest) error {
	if err := requireText("--to", r.To); err != nil {
		return err
	}
	if _, err := parseUserJID(r.To); err != nil {
		return err
	}
	if r.SubAction != "block" && r.SubAction != "unblock" {
		return fmt.Errorf("unsupported action %q (block|unblock)", r.SubAction)
	}
	return nil
}

func runContactBlock(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseUserJID(r.To)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	block := r.SubAction == "block"
	if _, err := a.WA().UpdateBlocklist(ctx, jid, block); err != nil {
		return nil, fmt.Errorf("%s contact: %w", r.SubAction, err)
	}
	return actionAck{JID: jid.String(), Blocked: boolPtr(block)}, nil
}

func runBlocklist(ctx context.Context, a *app.App, _ sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	list, err := a.WA().GetBlocklist(ctx)
	if err != nil {
		return nil, fmt.Errorf("get blocklist: %w", err)
	}
	out := actionAck{JIDs: []string{}}
	if list != nil {
		for _, it := range list.Items {
			if !it.Active && it.PN.IsEmpty() && it.LID.IsEmpty() {
				continue
			}
			j := it.PN
			if j.IsEmpty() {
				j = it.LID
			}
			out.JIDs = append(out.JIDs, j.ToNonAD().String())
		}
	}
	return out, nil
}

// ---- groups: photo ----

func validateGroupPhoto(r sendDelegateRequest) error {
	if err := validateGroupJID(r); err != nil {
		return err
	}
	if r.SubAction == "remove" {
		if len(r.Picture) != 0 {
			return fmt.Errorf("a picture cannot be sent with remove")
		}
		return nil
	}
	if r.SubAction != "" && r.SubAction != "set" {
		return fmt.Errorf("unsupported action %q (set|remove)", r.SubAction)
	}
	return validateJPEG(r.Picture)
}

func validateJPEG(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("picture is required")
	}
	if len(b) > profileMaxPictureBytes {
		return fmt.Errorf("picture is larger than %d bytes", profileMaxPictureBytes)
	}
	if len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 {
		return fmt.Errorf("picture must be a JPEG")
	}
	return nil
}

func runGroupPhoto(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseGroupJID(r.GroupJID)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	var pic []byte
	if r.SubAction != "remove" {
		pic = r.Picture
	}
	id, err := a.WA().SetGroupPhoto(ctx, jid, pic)
	if err != nil {
		return nil, fmt.Errorf("set group photo: %w", err)
	}
	return actionAck{JID: jid.String(), PictureID: id}, nil
}

// ---- disappearing messages ----

func parseDisappearingTimer(s string) (time.Duration, error) {
	switch strings.TrimSpace(s) {
	case "off":
		return 0, nil
	case "24h":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "90d":
		return 90 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unsupported duration %q (off|24h|7d|90d)", s)
}

func parseChatTarget(raw string) (types.JID, error) {
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		return types.JID{}, err
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return canonicalCLIJID(jid), nil
	}
	return types.JID{}, fmt.Errorf("expected a user or group chat, got %s", jid.String())
}

func validateChatDisappearing(r sendDelegateRequest) error {
	if err := requireText("--chat", r.Chat); err != nil {
		return err
	}
	if _, err := parseChatTarget(r.Chat); err != nil {
		return err
	}
	_, err := parseDisappearingTimer(r.EphemeralDuration)
	return err
}

func runChatDisappearing(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseChatTarget(r.Chat)
	dur, _ := parseDisappearingTimer(r.EphemeralDuration)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return nil, err
	}
	_, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (struct{}, error) {
		return struct{}{}, a.WA().SetDisappearingTimer(ctx, jid, dur)
	})
	if err != nil {
		return nil, fmt.Errorf("set disappearing timer: %w", err)
	}
	return actionAck{Chat: jid.String(), Duration: strings.TrimSpace(r.EphemeralDuration)}, nil
}

func runDisappearingDefault(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	dur, _ := parseDisappearingTimer(r.EphemeralDuration)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := a.WA().SetDefaultDisappearingTimer(ctx, dur); err != nil {
		return nil, fmt.Errorf("set default disappearing timer: %w", err)
	}
	return actionAck{Duration: strings.TrimSpace(r.EphemeralDuration)}, nil
}

// ---- status (stories) ----

func validateStatusPost(r sendDelegateRequest) error {
	if strings.TrimSpace(r.File) == "" {
		if strings.TrimSpace(r.Message) == "" {
			return fmt.Errorf("a status needs text (--message) or a file")
		}
	}
	if utf8.RuneCountInString(r.Message) > 4096 {
		return fmt.Errorf("status text is longer than 4096 characters")
	}
	if r.File != "" && (strings.TrimSpace(r.BackgroundColor) != "" || r.Font != nil) {
		return fmt.Errorf("background color and font apply to a text status only")
	}
	if strings.TrimSpace(r.BackgroundColor) != "" {
		if _, err := parseStatusBackgroundColor(r.BackgroundColor); err != nil {
			return err
		}
	}
	return nil
}

func runStatusPost(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	// A status is stored in status_messages, not messages, so the shared retry guard does not see it. A repeated
	// id that is already stored means the first attempt went out.
	if r.NewID != "" {
		if st, err := a.DB().GetStatusMessage(r.NewID); err == nil && st.FromMe {
			return sentAck{actionAck{sendMeta: sendMeta{AlreadySent: true}, ID: r.NewID, Chat: types.StatusBroadcastJID.String()}}, nil
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.File) != "" {
		res, err := runFileSendOperation(ctx, r.File, reconnectForSend(a), func(ctx context.Context) (sendFileOutcome, error) {
			return sendFile(ctx, a, types.StatusBroadcastJID, r.File, sendFileOptions{caption: r.Message, mimeOverride: r.MIME})
		})
		if err != nil {
			return nil, err
		}
		return sentAck{actionAck{ID: res.id, Chat: types.StatusBroadcastJID.String()}}, nil
	}
	var font *int32
	if r.Font != nil {
		font = r.Font
	}
	msg, err := buildStatusTextMessage(r.Message, statusTextOptions{BackgroundColor: r.BackgroundColor, Font: font})
	if err != nil {
		return nil, err
	}
	id, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return sendStatusTextMessage(ctx, a.WA(), msg)
	})
	if err != nil {
		return nil, err
	}
	var storedFont int32
	if font != nil {
		storedFont = *font
	}
	if err := a.DB().UpsertStatusMessage(store.UpsertStatusMessageParams{
		MsgID: string(id), Timestamp: time.Now().UTC(), FromMe: true, Text: r.Message,
		BackgroundColor: r.BackgroundColor, Font: storedFont,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: status %s sent but not stored: %v\n", id, err)
	}
	return sentAck{actionAck{ID: string(id), Chat: types.StatusBroadcastJID.String()}}, nil
}

func runStatusDelete(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := warnRapidSendIfNeeded(a.StoreDir(), time.Now().UTC(), os.Stderr); err != nil {
		return nil, err
	}
	id, err := runSendOperation(ctx, reconnectForSend(a), func(ctx context.Context) (types.MessageID, error) {
		return a.WA().RevokeMessage(ctx, types.StatusBroadcastJID, types.MessageID(r.ID))
	})
	if err != nil {
		return nil, err
	}
	return sentAck{actionAck{ID: string(id), Target: r.ID, Chat: types.StatusBroadcastJID.String(), Deleted: boolPtr(true)}}, nil
}

// ---- channels ----

func validateChannelMute(r sendDelegateRequest) error {
	if err := requireText("--to", r.To); err != nil {
		return err
	}
	if _, err := parseChannelJID(r.To); err != nil {
		return err
	}
	if r.Enable == nil {
		return fmt.Errorf("enable is required (true mutes, false unmutes)")
	}
	return nil
}

func runChannelMute(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseChannelJID(r.To)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := a.WA().NewsletterToggleMute(ctx, jid, *r.Enable); err != nil {
		return nil, fmt.Errorf("mute channel: %w", err)
	}
	return actionAck{JID: jid.String(), Muted: boolPtr(*r.Enable)}, nil
}

func validateChannelCreate(r sendDelegateRequest) error {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if utf8.RuneCountInString(name) > 100 {
		return fmt.Errorf("channel name is longer than 100 characters")
	}
	if utf8.RuneCountInString(r.About) > 512 {
		return fmt.Errorf("channel description is longer than 512 characters")
	}
	return nil
}

func runChannelCreate(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	meta, err := a.WA().CreateNewsletter(ctx, strings.TrimSpace(r.Name), strings.TrimSpace(r.About))
	if err != nil {
		return nil, fmt.Errorf("create channel: %w", err)
	}
	if meta == nil {
		return nil, fmt.Errorf("create channel: no channel in the answer")
	}
	row := channelRecordFromMeta(meta)
	if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: channel %s created but not stored: %v\n", row.JID, err)
	}
	return actionAck{JID: row.JID, Name: row.Name}, nil
}

// ---- chats: delete ----

func validateChatDelete(r sendDelegateRequest) error {
	if err := requireText("--chat", r.Chat); err != nil {
		return err
	}
	_, err := parseChatTarget(r.Chat)
	return err
}

func runChatDelete(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseChatTarget(r.Chat)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := a.DeleteChatRemote(ctx, jid, r.DeleteMedia); err != nil {
		return nil, fmt.Errorf("delete chat: %w", err)
	}
	return actionAck{Chat: jid.String(), Deleted: boolPtr(true)}, nil
}

// ---- privacy ----

// privacyValues lists the values WhatsApp accepts for each setting; the daemon refuses anything else before the
// IQ, because a wrong value on a privacy setting can leave the account more open than the person meant.
var privacyValues = map[types.PrivacySettingType][]types.PrivacySetting{
	types.PrivacySettingTypeGroupAdd:     {types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
	types.PrivacySettingTypeLastSeen:     {types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
	types.PrivacySettingTypeStatus:       {types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
	types.PrivacySettingTypeProfile:      {types.PrivacySettingAll, types.PrivacySettingContacts, types.PrivacySettingContactBlacklist, types.PrivacySettingNone},
	types.PrivacySettingTypeReadReceipts: {types.PrivacySettingAll, types.PrivacySettingNone},
	types.PrivacySettingTypeOnline:       {types.PrivacySettingAll, types.PrivacySettingMatchLastSeen},
	types.PrivacySettingTypeCallAdd:      {types.PrivacySettingAll, types.PrivacySettingKnown},
	types.PrivacySettingTypeMessages:     {types.PrivacySettingAll, types.PrivacySettingContacts},
	types.PrivacySettingTypeDefense:      {types.PrivacySettingOnStandard, types.PrivacySettingOff},
	types.PrivacySettingTypeStickers:     {types.PrivacySettingContacts, types.PrivacySettingContactAllowlist, types.PrivacySettingNone},
}

func validatePrivacySet(r sendDelegateRequest) error {
	allowed, ok := privacyValues[types.PrivacySettingType(r.Name)]
	if !ok {
		return fmt.Errorf("unsupported privacy setting %q", r.Name)
	}
	for _, v := range allowed {
		if string(v) == r.Label {
			return nil
		}
	}
	return fmt.Errorf("value %q is not valid for %s (allowed: %v)", r.Label, r.Name, allowed)
}

func runPrivacyGet(ctx context.Context, a *app.App, _ sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	s, err := a.WA().GetPrivacySettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get privacy settings: %w", err)
	}
	return actionAck{Settings: privacyView(s)}, nil
}

func runPrivacySet(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	s, err := a.WA().SetPrivacySetting(ctx, types.PrivacySettingType(r.Name), types.PrivacySetting(r.Label))
	if err != nil {
		return nil, fmt.Errorf("set privacy %s: %w", r.Name, err)
	}
	return actionAck{Name: r.Name, Settings: privacyView(s)}, nil
}

func privacyView(s types.PrivacySettings) map[string]string {
	return map[string]string{
		"groupadd": string(s.GroupAdd), "last": string(s.LastSeen), "status": string(s.Status), "profile": string(s.Profile),
		"readreceipts": string(s.ReadReceipts), "online": string(s.Online), "calladd": string(s.CallAdd),
		"messages": string(s.Messages), "defense": string(s.Defense), "stickers": string(s.Stickers),
	}
}

// ---- calls ----

func validateCallReject(r sendDelegateRequest) error {
	if err := requireText("--to", r.To); err != nil {
		return err
	}
	if _, err := parseUserJID(r.To); err != nil {
		return err
	}
	return validateTargetMessageID("--id", r.ID)
}

func runCallReject(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, _ := parseUserJID(r.To)
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	if err := a.WA().RejectCall(ctx, jid, r.ID); err != nil {
		return nil, fmt.Errorf("reject call: %w", err)
	}
	return actionAck{JID: jid.String(), Target: r.ID, Rejected: boolPtr(true)}, nil
}
