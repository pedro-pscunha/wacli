package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/types"
)

// The daemon fixture runs the real socket server and executor with the fake
// WhatsApp client in daemon mode: the path registry handlers run on in
// production, which the socket-echo tests never reach.
type daemonFixture struct {
	app  *app.App
	fake *daemonFakeWA
	dir  string
	stop func()
	conn func(sendDelegateRequest) sendDelegateResponse
}

func newDaemonFixture(t *testing.T, f *daemonFakeWA) *daemonFixture {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	dir := shortPresenceDelegateStoreDir(t)
	a := newDaemonAppAt(t, f, dir)
	stop, err := startSendDelegateServerForStore(context.Background(), dir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		return executeDelegatedSend(ctx, a, req)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	fx := &daemonFixture{app: a, fake: f, dir: dir, stop: stop}
	fx.conn = func(req sendDelegateRequest) sendDelegateResponse {
		c, err := net.Dial("unix", sendDelegateSocketPath(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		req.Version = sendDelegateVersion
		if req.TimeoutMS == 0 {
			req.TimeoutMS = 8000
		}
		if err := json.NewEncoder(c).Encode(req); err != nil {
			t.Fatal(err)
		}
		var resp sendDelegateResponse
		if err := json.NewDecoder(c).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	return fx
}

func (fx *daemonFixture) call(t *testing.T, req sendDelegateRequest) sendDelegateResponse {
	t.Helper()
	return fx.conn(req)
}

func (fx *daemonFixture) noRedial(t *testing.T) {
	t.Helper()
	if c, d := fx.fake.calls(); c != 0 || d != 0 {
		t.Fatalf("the daemon redialed: connectCalls=%d disconnects=%d", c, d)
	}
}

func seedOwnMessage(t *testing.T, fx *daemonFixture, a *store.DB, chat, id string, fromMe bool) {
	t.Helper()
	_ = a.UpsertChat(chat, "dm", "x", time.Now())
	if err := a.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: id, Timestamp: time.Now(), FromMe: fromMe, Text: "seed"}); err != nil {
		t.Fatal(err)
	}
}

const chatA = "5511999999999@s.whatsapp.net"

func TestDaemonRunsMessageKinds(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	db := fx.app.DB()
	seedOwnMessage(t, fx, db, chatA, "ORIGINAL0001", true)

	// forward: a new message is stored in the target chat under the new id.
	resp := fx.call(t, sendDelegateRequest{Kind: forwardKind, Chat: chatA, ID: "ORIGINAL0001", To: "5511888888888@s.whatsapp.net", NewID: "FORWARDEDID01"})
	if !resp.OK || !resp.Sent || resp.ID != "FORWARDEDID01" {
		t.Fatalf("forward = %+v", resp)
	}
	if m, err := db.GetMessage("5511888888888@s.whatsapp.net", "FORWARDEDID01"); err != nil || !m.FromMe || !m.IsForwarded {
		t.Fatalf("forwarded message not stored: %+v err=%v", m, err)
	}

	// revoke: the target is marked revoked in the store.
	resp = fx.call(t, sendDelegateRequest{Kind: revokeKind, Chat: chatA, ID: "ORIGINAL0001"})
	if !resp.OK || resp.ID == "" {
		t.Fatalf("revoke = %+v", resp)
	}
	if m, err := db.GetMessage(chatA, "ORIGINAL0001"); err != nil || !m.Revoked {
		t.Fatalf("revoke not stored: %+v err=%v", m, err)
	}

	// delete for me: marked in the store, sent through the app-state gate.
	seedOwnMessage(t, fx, db, chatA, "TODELETE0001", false)
	resp = fx.call(t, sendDelegateRequest{Kind: deleteForMeKind, Chat: chatA, ID: "TODELETE0001"})
	if !resp.OK {
		t.Fatalf("delete_for_me = %+v", resp)
	}
	if m, err := db.GetMessage(chatA, "TODELETE0001"); err != nil || !m.DeletedForMe {
		t.Fatalf("delete-for-me not stored: %+v err=%v", m, err)
	}
	if f.deletesForMe != 1 || f.revokes != 1 || len(f.protoSends) != 1 {
		t.Fatalf("wire calls: delete=%d revoke=%d proto=%d", f.deletesForMe, f.revokes, len(f.protoSends))
	}
	fx.noRedial(t)
}

func TestDaemonRunsGroupProfileChannelAndReadKinds(t *testing.T) {
	f := newDaemonFakeWA()
	gjid, _ := types.ParseJID("120363000000000001@g.us")
	f.joinedGroups = []*types.GroupInfo{{JID: gjid, GroupName: types.GroupName{Name: "Obra A"}}}
	njid, _ := types.ParseJID("120363000000000002@newsletter")
	f.newsletters = []*types.NewsletterMetadata{{ID: njid}}
	fx := newDaemonFixture(t, f)

	if r := fx.call(t, sendDelegateRequest{Kind: groupRenameKind, GroupJID: gjid.String(), Name: "Obra B"}); !r.OK {
		t.Fatalf("group_rename = %+v", r)
	}
	if len(f.groupNames) != 1 || f.groupNames[0] != "Obra B" {
		t.Fatalf("group names = %v", f.groupNames)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: groupRefreshKind}); !r.OK || string(r.Data) != `{"groups":1}` {
		t.Fatalf("group_refresh = %+v", r)
	}
	if g, err := fx.app.DB().ListGroups("", 10); err != nil || len(g) != 1 || g[0].Name != "Obra A" {
		t.Fatalf("groups not stored: %+v err=%v", g, err)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: profileSetNameKind, Name: "Pedro"}); !r.OK || f.profileNames[0] != "Pedro" {
		t.Fatalf("profile_set_name = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: profileSetAboutKind, About: "ocupado"}); !r.OK || f.profileAbouts[0] != "ocupado" {
		t.Fatalf("profile_set_about = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: channelsListKind}); !r.OK {
		t.Fatalf("channels_list = %+v", r)
	}
	if ch, err := fx.app.DB().GetChat(njid.String()); err != nil || ch.Kind != "newsletter" {
		t.Fatalf("channel not stored: %+v err=%v", ch, err)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: channelsLeaveKind, Chat: njid.String()}); !r.OK || len(f.followed) != 1 {
		t.Fatalf("channels_leave = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: doctorKind}); !r.OK || string(r.Data) == "" {
		t.Fatalf("doctor = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: devicesKind}); !r.OK || len(r.Devices) != 1 {
		t.Fatalf("devices = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: capabilitiesKind}); !r.OK || r.Capabilities == nil {
		t.Fatalf("capabilities = %+v", r)
	}
	fx.noRedial(t)
}

// When the daemon's socket to WhatsApp is down, a handler fails clearly and
// never dials its own connection.
func TestDaemonKindsRefuseWhenDisconnectedAndNeverDial(t *testing.T) {
	f := newDaemonFakeWA()
	f.setConnected(false)
	fx := newDaemonFixture(t, f)
	for _, req := range []sendDelegateRequest{
		{Kind: groupRenameKind, GroupJID: "120363000000000001@g.us", Name: "x"},
		{Kind: profileSetNameKind, Name: "x"},
		{Kind: channelsListKind},
		{Kind: groupRefreshKind},
	} {
		if r := fx.call(t, req); r.OK {
			t.Fatalf("%s succeeded with the daemon disconnected: %+v", req.Kind, r)
		}
	}
	fx.noRedial(t)
}
