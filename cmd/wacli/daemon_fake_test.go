package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/wa"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
)

// daemonFakeWA stands in for the daemon's WhatsApp client. The embedded
// interface is nil on purpose: a test that reaches a method it did not
// override panics, which names the gap.
type daemonFakeWA struct {
	app.WAClient
	mu           sync.Mutex
	connected    bool
	connectCalls int
	disconnects  int
	handlers     map[uint32]func(any)
	nextHandler  uint32

	sendTextErrs []error // consumed one per SendText call
	// errBeforeWire makes an injected SendText error come before the dispatch mark.
	errBeforeWire bool
	sendTexts     []string
	sendIDs       []types.MessageID
	idsSeen       []types.MessageID
	revokes       int

	protoSends     []types.JID
	deletesForMe   int
	groupNames     []string
	profileNames   []string
	profileAbouts  []string
	followed       []types.JID
	joinedGroups   []*types.GroupInfo
	newsletters    []*types.NewsletterMetadata
	historyRequest int

	// rewriteOnWhatsApp models the registration check returning a different JID
	// than the typed number (the Brazilian ninth digit).
	rewriteOnWhatsApp func(phone string) (types.JID, bool)

	actions       actionLog
	userInfoCalls int
	lidMap        map[string]string // lid user -> pn user, learned while the daemon runs
}

func newDaemonFakeWA() *daemonFakeWA {
	return &daemonFakeWA{connected: true, handlers: map[uint32]func(any){}}
}

func (f *daemonFakeWA) AddEventHandler(h func(any)) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextHandler++
	f.handlers[f.nextHandler] = h
	return f.nextHandler
}
func (f *daemonFakeWA) RemoveEventHandler(id uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.handlers, id)
}
func (f *daemonFakeWA) IsAuthed() bool { return true }
func (f *daemonFakeWA) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}
func (f *daemonFakeWA) setConnected(v bool) { f.mu.Lock(); f.connected = v; f.mu.Unlock() }
func (f *daemonFakeWA) Disconnect()         { f.mu.Lock(); f.disconnects++; f.connected = false; f.mu.Unlock() }
func (f *daemonFakeWA) Close()              {}
func (f *daemonFakeWA) Connect(ctx context.Context, opts wa.ConnectOptions) error {
	f.mu.Lock()
	f.connectCalls++
	f.connected = true
	f.mu.Unlock()
	return nil
}
func (f *daemonFakeWA) calls() (connects, disconnects int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls, f.disconnects
}
func (f *daemonFakeWA) LinkedJID() string                                         { return "5511000000000@s.whatsapp.net" }
func (f *daemonFakeWA) LinkedLID() string                                         { return "" }
func (f *daemonFakeWA) ResolveChatName(context.Context, types.JID, string) string { return "chat" }

// learnLID models whatsmeow learning a LID/PN mapping while the daemon runs.
func (f *daemonFakeWA) learnLID(lid, pn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lidMap == nil {
		f.lidMap = map[string]string{}
	}
	f.lidMap[lid] = pn
}

// ResolvePNToLIDStoreOnly reads the mapping only. ResolvePNToLID counts a user-info query on a miss, as the real
// client does.
func (f *daemonFakeWA) ResolvePNToLIDStoreOnly(_ context.Context, j types.JID) types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupPNLocked(j)
}
func (f *daemonFakeWA) ResolvePNToLID(_ context.Context, j types.JID) types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.lookupPNLocked(j); got != j {
		return got
	}
	f.userInfoCalls++
	return j
}
func (f *daemonFakeWA) lookupPNLocked(j types.JID) types.JID {
	for lid, pn := range f.lidMap {
		if j.Server == types.DefaultUserServer && j.User == pn {
			return types.JID{User: lid, Server: types.HiddenUserServer}
		}
	}
	return j
}
func (f *daemonFakeWA) ResolveLIDToPN(_ context.Context, j types.JID) types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pn, ok := f.lidMap[j.User]; ok && j.Server == types.HiddenUserServer {
		return types.JID{User: pn, Server: types.DefaultUserServer}
	}
	return j
}
func (f *daemonFakeWA) GetUserInfo(context.Context, []types.JID) (map[types.JID]types.UserInfo, error) {
	return nil, nil
}
func (f *daemonFakeWA) IsOnWhatsApp(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
	if f.rewriteOnWhatsApp == nil {
		return nil, nil
	}
	var out []types.IsOnWhatsAppResponse
	for _, p := range phones {
		if jid, ok := f.rewriteOnWhatsApp(p); ok {
			out = append(out, types.IsOnWhatsAppResponse{Query: p, JID: jid, IsIn: true})
		}
	}
	return out, nil
}
func (f *daemonFakeWA) GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error) {
	return nil, nil
}
func (f *daemonFakeWA) SendText(ctx context.Context, to types.JID, text string) (types.MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idsSeen = append(f.idsSeen, wa.MessageIDFromContext(ctx))
	if len(f.sendTextErrs) > 0 {
		err := f.sendTextErrs[0]
		f.sendTextErrs = f.sendTextErrs[1:]
		if err != nil {
			// An error injected as "after dispatch" models the real client: the
			// mark is set right before SendMessage, so the error comes after it.
			if !f.errBeforeWire {
				wa.SendPhaseFrom(ctx).MarkDispatched()
			}
			return "", err
		}
	}
	wa.SendPhaseFrom(ctx).MarkDispatched()
	f.sendTexts = append(f.sendTexts, text)
	if id := wa.MessageIDFromContext(ctx); id != "" {
		return id, nil
	}
	return types.MessageID("RANDOMID0001"), nil
}

// newDaemonApp builds the app the sync daemon would hold: a writable store, the
// fake client, and DelegateOnly off so that a stray Connect is counted by the
// fake rather than refused.
func newDaemonApp(t *testing.T, f *daemonFakeWA) *app.App {
	t.Helper()
	return newDaemonAppAt(t, f, t.TempDir())
}

func newDaemonAppAt(t *testing.T, f *daemonFakeWA, dir string) *app.App {
	t.Helper()
	a, err := app.New(app.Options{StoreDir: dir, WA: f})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	if err := a.OpenWA(); err != nil {
		t.Fatalf("OpenWA: %v", err)
	}
	return a
}

func fastDaemonPolling(t *testing.T) {
	t.Helper()
	old := daemonConnectionPollInterval
	daemonConnectionPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { daemonConnectionPollInterval = old })
}

func (f *daemonFakeWA) SendProtoMessage(ctx context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protoSends = append(f.protoSends, to)
	f.idsSeen = append(f.idsSeen, wa.MessageIDFromContext(ctx))
	if id := wa.MessageIDFromContext(ctx); id != "" {
		return id, nil
	}
	return types.MessageID("PROTOID000001"), nil
}
func (f *daemonFakeWA) RevokeMessage(ctx context.Context, chat types.JID, target types.MessageID) (types.MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes++
	if id := wa.MessageIDFromContext(ctx); id != "" {
		return id, nil
	}
	return types.MessageID("REVOKEID00001"), nil
}
func (f *daemonFakeWA) DeleteMessageForMe(ctx context.Context, info types.MessageInfo, deleteMedia bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletesForMe++
	return nil
}
func (f *daemonFakeWA) ArchiveChat(ctx context.Context, target types.JID, archive bool, ts time.Time, key *waCommon.MessageKey, before func()) ([]any, error) {
	return nil, nil
}
func (f *daemonFakeWA) SetGroupName(ctx context.Context, jid types.JID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupNames = append(f.groupNames, name)
	return nil
}
func (f *daemonFakeWA) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.joinedGroups, nil
}
func (f *daemonFakeWA) SetProfileName(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileNames = append(f.profileNames, name)
	return nil
}
func (f *daemonFakeWA) SetStatusMessage(ctx context.Context, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profileAbouts = append(f.profileAbouts, msg)
	return nil
}
func (f *daemonFakeWA) GetSubscribedNewsletters(context.Context) ([]*types.NewsletterMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newsletters, nil
}
func (f *daemonFakeWA) UnfollowNewsletter(ctx context.Context, jid types.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followed = append(f.followed, jid)
	return nil
}
func (f *daemonFakeWA) OwnDevices(context.Context) ([]wa.OwnDevice, error) {
	return []wa.OwnDevice{{JID: "5511000000000@s.whatsapp.net", Self: true}}, nil
}
func (f *daemonFakeWA) FetchAppStateEvents(context.Context, string, bool, bool) ([]any, error) {
	return nil, nil
}
func (f *daemonFakeWA) HasLocalAppState(context.Context, string) (bool, error) { return true, nil }

// actionLog records the account actions of delegate_actions.go.
type actionLog struct {
	stars, pins, blocks, unblocks, photos, joinApprovals, memberAdds, disappearing, defaultDisappearing int
	audienceReads                                                                                       int
	mutes, creates, deletes, privacySets, privacyGets, rejects, blocklistReads                          int
	lastPinDuration                                                                                     time.Duration
	lastPinFromMe                                                                                       bool
	lastTimer                                                                                           time.Duration
	lastPrivacy                                                                                         [2]string
	lastBlocked                                                                                         types.JID
	allMembers                                                                                          bool
}

func (f *daemonFakeWA) StarMessage(_ context.Context, _ types.MessageInfo, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.stars++
	return nil
}
func (f *daemonFakeWA) PinInChat(ctx context.Context, _ types.JID, _ types.JID, fromMe bool, _ types.MessageID, _ bool, d time.Duration) (types.MessageID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.pins++
	f.actions.lastPinDuration, f.actions.lastPinFromMe = d, fromMe
	f.idsSeen = append(f.idsSeen, wa.MessageIDFromContext(ctx))
	if id := wa.MessageIDFromContext(ctx); id != "" {
		return id, nil
	}
	return "PINMSG000001", nil
}
func (f *daemonFakeWA) UpdateBlocklist(_ context.Context, j types.JID, block bool) (*types.Blocklist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if block {
		f.actions.blocks++
	} else {
		f.actions.unblocks++
	}
	f.actions.lastBlocked = j
	return &types.Blocklist{}, nil
}
func (f *daemonFakeWA) GetBlocklist(context.Context) (*types.Blocklist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.blocklistReads++
	return &types.Blocklist{Items: []types.BlocklistItem{{PN: types.NewJID("5511777777777", types.DefaultUserServer), Active: true}}}, nil
}
func (f *daemonFakeWA) SetGroupPhoto(_ context.Context, _ types.JID, b []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.photos++
	if b == nil {
		return "remove", nil
	}
	return "pic1", nil
}
func (f *daemonFakeWA) SetGroupJoinApprovalMode(context.Context, types.JID, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.joinApprovals++
	return nil
}
func (f *daemonFakeWA) SetGroupMemberAddMode(_ context.Context, _ types.JID, all bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.memberAdds++
	f.actions.allMembers = all
	return nil
}
func (f *daemonFakeWA) SetDisappearingTimer(_ context.Context, _ types.JID, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.disappearing++
	f.actions.lastTimer = d
	return nil
}
func (f *daemonFakeWA) SetDefaultDisappearingTimer(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.defaultDisappearing++
	f.actions.lastTimer = d
	return nil
}
func (f *daemonFakeWA) NewsletterToggleMute(context.Context, types.JID, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.mutes++
	return nil
}
func (f *daemonFakeWA) CreateNewsletter(_ context.Context, name, _ string) (*types.NewsletterMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.creates++
	return &types.NewsletterMetadata{ID: types.NewJID("120363999999999999", types.NewsletterServer)}, nil
}
func (f *daemonFakeWA) DeleteChat(context.Context, types.JID, time.Time, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.deletes++
	return nil
}
func (f *daemonFakeWA) GetPrivacySettings(context.Context) (types.PrivacySettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.privacyGets++
	return types.PrivacySettings{LastSeen: types.PrivacySettingContacts}, nil
}
func (f *daemonFakeWA) SetPrivacySetting(_ context.Context, name types.PrivacySettingType, v types.PrivacySetting) (types.PrivacySettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.privacySets++
	f.actions.lastPrivacy = [2]string{string(name), string(v)}
	return types.PrivacySettings{LastSeen: v}, nil
}
func (f *daemonFakeWA) RejectCall(context.Context, types.JID, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.rejects++
	return nil
}

func (f *daemonFakeWA) StatusAudience(context.Context) (types.StatusPrivacyType, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions.audienceReads++
	return types.StatusPrivacyTypeContacts, 42, nil
}
