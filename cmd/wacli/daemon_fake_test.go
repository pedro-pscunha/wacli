package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/wa"
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
	sendTexts    []string
	sendIDs      []types.MessageID
	idsSeen      []types.MessageID
	revokes      int
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
func (f *daemonFakeWA) emit(evt any) {
	f.mu.Lock()
	hs := make([]func(any), 0, len(f.handlers))
	for _, h := range f.handlers {
		hs = append(hs, h)
	}
	f.mu.Unlock()
	for _, h := range hs {
		h(evt)
	}
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
func (f *daemonFakeWA) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	return nil, nil
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
			return "", err
		}
	}
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
	a, err := app.New(app.Options{StoreDir: t.TempDir(), WA: f})
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
