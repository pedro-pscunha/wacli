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
func (f *daemonFakeWA) ResolvePNToLID(_ context.Context, j types.JID) types.JID   { return j }
func (f *daemonFakeWA) ResolveLIDToPN(_ context.Context, j types.JID) types.JID   { return j }
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
				wa.MarkDispatched(ctx)
			}
			return "", err
		}
	}
	wa.MarkDispatched(ctx)
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
