package main

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

const (
	actionDM     = "5511999999999@s.whatsapp.net"
	actionGroup  = "120363000000000001@g.us"
	actionChanel = "120363000000000002@newsletter"
)

func jpegBytes(n int) []byte {
	b := make([]byte, n)
	b[0], b[1], b[2] = 0xFF, 0xD8, 0xFF
	return b
}

func tr(b bool) *bool { return &b }

func realJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every new kind refuses bad input before any I/O, and the daemon runs the good ones on its connection.
func TestActionKindValidators(t *testing.T) {
	cases := map[string]struct {
		req sendDelegateRequest
		bad string // substring of the expected error; empty means valid
	}{
		"star ok":                {sendDelegateRequest{Kind: messageStarKind, Chat: actionDM, ID: "ABC12345", Enable: tr(true)}, ""},
		"star no enable":         {sendDelegateRequest{Kind: messageStarKind, Chat: actionDM, ID: "ABC12345"}, "enable"},
		"star no id":             {sendDelegateRequest{Kind: messageStarKind, Chat: actionDM, Enable: tr(true)}, "--id"},
		"pin ok":                 {sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "ABC12345", Enable: tr(true), EphemeralDuration: "7d"}, ""},
		"pin bad duration":       {sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "ABC12345", Enable: tr(true), EphemeralDuration: "3d"}, "pin duration"},
		"pin missing duration":   {sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "ABC12345", Enable: tr(true)}, "pin duration"},
		"unpin ok":               {sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "ABC12345", Enable: tr(false)}, ""},
		"unpin with duration":    {sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "ABC12345", Enable: tr(false), EphemeralDuration: "7d"}, "only applies to a pin"},
		"block ok":               {sendDelegateRequest{Kind: contactBlockKind, To: "+55 11 99999-9999", SubAction: "block"}, ""},
		"block group jid":        {sendDelegateRequest{Kind: contactBlockKind, To: actionGroup, SubAction: "block"}, "phone number or a user JID"},
		"block bad action":       {sendDelegateRequest{Kind: contactBlockKind, To: actionDM, SubAction: "mute"}, "block|unblock"},
		"photo ok":               {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: realJPEG(t)}, ""},
		"photo remove ok":        {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, SubAction: "remove"}, ""},
		"photo remove with data": {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, SubAction: "remove", Picture: jpegBytes(10)}, "cannot be sent"},
		"photo png":              {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: []byte{0x89, 'P', 'N', 'G'}}, "JPEG"},
		"photo too big":          {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: jpegBytes(profileMaxPictureBytes + 1)}, "larger than"},
		"photo dm jid":           {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionDM, Picture: jpegBytes(10)}, "group JID"},
		"setting join-approval":  {sendDelegateRequest{Kind: groupSettingKind, GroupJID: actionGroup, SubAction: "join-approval", Enable: tr(true)}, ""},
		"setting member-add":     {sendDelegateRequest{Kind: groupSettingKind, GroupJID: actionGroup, SubAction: "member-add", Enable: tr(false)}, ""},
		"setting unknown":        {sendDelegateRequest{Kind: groupSettingKind, GroupJID: actionGroup, SubAction: "owner", Enable: tr(true)}, "unsupported group setting"},
		"disappearing ok":        {sendDelegateRequest{Kind: chatDisappearingKind, Chat: actionGroup, EphemeralDuration: "90d"}, ""},
		"disappearing off":       {sendDelegateRequest{Kind: chatDisappearingKind, Chat: actionDM, EphemeralDuration: "off"}, ""},
		"disappearing 3 days":    {sendDelegateRequest{Kind: chatDisappearingKind, Chat: actionDM, EphemeralDuration: "3d"}, "unsupported duration"},
		"disappearing channel":   {sendDelegateRequest{Kind: chatDisappearingKind, Chat: actionChanel, EphemeralDuration: "7d"}, "user or group chat"},
		"default timer bad":      {sendDelegateRequest{Kind: disappearingDefaultKd, EphemeralDuration: "1h"}, "unsupported duration"},
		"status text":            {sendDelegateRequest{Kind: statusPostKind, Message: "hello"}, ""},
		"status empty":           {sendDelegateRequest{Kind: statusPostKind}, "needs text"},
		"status bad color":       {sendDelegateRequest{Kind: statusPostKind, Message: "x", BackgroundColor: "red"}, "background-color"},
		"status file with color": {sendDelegateRequest{Kind: statusPostKind, File: "/tmp/a.jpg", BackgroundColor: "#112233"}, "text status only"},
		"status relative file":   {sendDelegateRequest{Kind: statusPostKind, File: "foto.jpg"}, "absolute path"},
		"file kind relative":     {sendDelegateRequest{Kind: "file", To: actionDM, File: "foto.jpg"}, "absolute path"},
		"photo fake jpeg":        {sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: jpegBytes(10)}, "JPEG"},
		"status delete no id":    {sendDelegateRequest{Kind: statusDeleteKind}, "--id"},
		"channel mute ok":        {sendDelegateRequest{Kind: channelMuteKind, To: actionChanel, Enable: tr(true)}, ""},
		"channel mute group":     {sendDelegateRequest{Kind: channelMuteKind, To: actionGroup, Enable: tr(true)}, "channel"},
		"channel create ok":      {sendDelegateRequest{Kind: channelCreateKind, Name: "Obras", About: "novidades"}, ""},
		"channel create blank":   {sendDelegateRequest{Kind: channelCreateKind, Name: "  "}, "--name"},
		"channel create long":    {sendDelegateRequest{Kind: channelCreateKind, Name: strings.Repeat("a", 101)}, "longer than 100"},
		"chat delete ok":         {sendDelegateRequest{Kind: chatDeleteKind, Chat: actionDM}, ""},
		"chat delete channel":    {sendDelegateRequest{Kind: chatDeleteKind, Chat: actionChanel}, "user or group chat"},
		"privacy set ok":         {sendDelegateRequest{Kind: privacySetKind, Name: "last", Label: "contacts"}, ""},
		"privacy unknown name":   {sendDelegateRequest{Kind: privacySetKind, Name: "everything", Label: "all"}, "unsupported privacy setting"},
		"privacy bad value":      {sendDelegateRequest{Kind: privacySetKind, Name: "readreceipts", Label: "contacts"}, "not valid"},
		"call reject ok":         {sendDelegateRequest{Kind: callRejectKind, To: actionDM, ID: "CALLID123456"}, ""},
		"call reject no id":      {sendDelegateRequest{Kind: callRejectKind, To: actionDM}, "--id"},
	}
	for name, c := range cases {
		err := validateDelegateRequest(c.req)
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.bad)
		}
	}
}

// The paced set is a decision about what counts as a message on the wire: pin it.
func TestActionKindsPacing(t *testing.T) {
	want := map[string]bool{}
	for _, k := range pacedActionKinds {
		want[k] = true
	}
	all := []string{messageStarKind, messagePinKind, contactBlockKind, blocklistKind, groupPhotoKind, chatDisappearingKind,
		disappearingDefaultKd, statusPostKind, statusDeleteKind, statusAudienceKind, channelMuteKind, channelCreateKind, chatDeleteKind,
		privacyGetKind, privacySetKind, callRejectKind}
	for _, k := range all {
		h, ok := delegateHandlers[k]
		if !ok {
			t.Fatalf("%s is not registered", k)
		}
		if h.localOK {
			t.Errorf("%s must not be localOK: it needs the connection", k)
		}
		if want[k] && h.skipsQueue {
			t.Errorf("%s puts a message on the wire and must take the paced slot", k)
		}
		if !want[k] && !h.skipsQueue {
			t.Errorf("%s sends no message and must skip the queue", k)
		}
	}
	if len(pacedActionKinds) != 4 {
		t.Fatalf("paced action kinds = %v, want exactly message_pin, chat_disappearing, status_post, status_delete", pacedActionKinds)
	}
}

func TestDaemonRunsActionKinds(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	db := fx.app.DB()
	seedOwnMessage(t, fx, db, actionDM, "TARGET000001", false)

	// star: app-state write, mirrored in the store
	if r := fx.call(t, sendDelegateRequest{Kind: messageStarKind, Chat: actionDM, ID: "TARGET000001", Enable: tr(true)}); !r.OK {
		t.Fatalf("star = %+v", r)
	}
	if f.actions.stars != 1 {
		t.Fatalf("stars = %d", f.actions.stars)
	}

	// pin: a message on the wire; duration travels; new id is used
	r := fx.call(t, sendDelegateRequest{Kind: messagePinKind, Chat: actionDM, ID: "TARGET000001", Enable: tr(true), EphemeralDuration: "7d", NewID: "PINNEWID0001"})
	if !r.OK || !r.Sent || r.ID != "PINNEWID0001" {
		t.Fatalf("pin = %+v", r)
	}
	if f.actions.pins != 1 || f.actions.lastPinDuration.Hours() != 168 {
		t.Fatalf("pin call = %+v", f.actions)
	}

	// block, blocklist
	if r := fx.call(t, sendDelegateRequest{Kind: contactBlockKind, To: "+55 11 99999-9999", SubAction: "block"}); !r.OK {
		t.Fatalf("block = %+v", r)
	}
	if f.actions.blocks != 1 || f.actions.lastBlocked.String() != actionDM {
		t.Fatalf("block call = %+v", f.actions)
	}
	r = fx.call(t, sendDelegateRequest{Kind: blocklistKind})
	if !r.OK || !strings.Contains(string(r.Data), "5511777777777@s.whatsapp.net") {
		t.Fatalf("blocklist = %+v data=%s", r, r.Data)
	}

	// group photo and the two new group settings
	if r := fx.call(t, sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: realJPEG(t)}); !r.OK || !strings.Contains(string(r.Data), "pic1") {
		t.Fatalf("photo = %+v data=%s", r, r.Data)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: groupPhotoKind, GroupJID: actionGroup, SubAction: "remove"}); !r.OK || !strings.Contains(string(r.Data), "remove") {
		t.Fatalf("photo remove = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: groupSettingKind, GroupJID: actionGroup, SubAction: "join-approval", Enable: tr(true)}); !r.OK {
		t.Fatalf("join-approval = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: groupSettingKind, GroupJID: actionGroup, SubAction: "member-add", Enable: tr(true)}); !r.OK {
		t.Fatalf("member-add = %+v", r)
	}
	if f.actions.joinApprovals != 1 || f.actions.memberAdds != 1 || !f.actions.allMembers {
		t.Fatalf("group settings = %+v", f.actions)
	}

	// disappearing timers
	if r := fx.call(t, sendDelegateRequest{Kind: chatDisappearingKind, Chat: actionDM, EphemeralDuration: "24h"}); !r.OK {
		t.Fatalf("disappearing = %+v", r)
	}
	if f.actions.disappearing != 1 || f.actions.lastTimer.Hours() != 24 {
		t.Fatalf("timer = %+v", f.actions)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: disappearingDefaultKd, EphemeralDuration: "off"}); !r.OK || f.actions.defaultDisappearing != 1 {
		t.Fatalf("default timer = %+v", r)
	}

	// channels
	if r := fx.call(t, sendDelegateRequest{Kind: channelMuteKind, To: actionChanel, Enable: tr(true)}); !r.OK || f.actions.mutes != 1 {
		t.Fatalf("channel mute = %+v", r)
	}
	r = fx.call(t, sendDelegateRequest{Kind: channelCreateKind, Name: "Obras"})
	if !r.OK || !strings.Contains(string(r.Data), "120363999999999999@newsletter") {
		t.Fatalf("channel create = %+v data=%s", r, r.Data)
	}

	// privacy and calls
	r = fx.call(t, sendDelegateRequest{Kind: privacySetKind, Name: "last", Label: "none"})
	if !r.OK || f.actions.lastPrivacy != [2]string{"last", "none"} {
		t.Fatalf("privacy set = %+v %+v", r, f.actions)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: privacyGetKind}); !r.OK || !strings.Contains(string(r.Data), `"last":"contacts"`) {
		t.Fatalf("privacy get = %+v data=%s", r, r.Data)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: callRejectKind, To: actionDM, ID: "CALLID123456"}); !r.OK || f.actions.rejects != 1 {
		t.Fatalf("call reject = %+v", r)
	}

	// delete chat: remote write, then the local copy is gone
	if r := fx.call(t, sendDelegateRequest{Kind: chatDeleteKind, Chat: actionDM}); !r.OK {
		t.Fatalf("chat delete = %+v", r)
	}
	if f.actions.deletes != 1 {
		t.Fatalf("deletes = %d", f.actions.deletes)
	}
	// The local archive stays, as it does when the chat is deleted on the phone.
	if _, err := db.GetMessage(actionDM, "TARGET000001"); err != nil {
		t.Fatalf("chat_delete removed the local archive: %v", err)
	}
	fx.noRedial(t)
}

func TestDaemonStatusPostAndDelete(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	r := fx.call(t, sendDelegateRequest{Kind: statusPostKind, Message: "bom dia", BackgroundColor: "#112233", NewID: "STATUSID0001"})
	if !r.OK || !r.Sent || r.ID != "STATUSID0001" {
		t.Fatalf("status post = %+v", r)
	}
	if len(f.protoSends) != 1 || f.protoSends[0] != types.StatusBroadcastJID {
		t.Fatalf("status was not sent to status@broadcast: %v", f.protoSends)
	}
	if st, err := fx.app.DB().GetStatusMessage("STATUSID0001"); err != nil || st.Text != "bom dia" || st.BackgroundColor != "#112233" {
		t.Fatalf("status not stored: %+v err=%v", st, err)
	}
	// the same id again: already sent, nothing on the wire
	r = fx.call(t, sendDelegateRequest{Kind: statusPostKind, Message: "bom dia", NewID: "STATUSID0001"})
	if !r.OK || !r.AlreadySent || len(f.protoSends) != 1 {
		t.Fatalf("repeat = %+v sends=%d, want already_sent and no new send", r, len(f.protoSends))
	}
	if !strings.Contains(string(fx.call(t, sendDelegateRequest{Kind: statusPostKind, Message: "outro", NewID: "STATUSID0002"}).Data), `"audience":{"type":"contacts","count":42}`) {
		t.Fatal("status_post did not report its audience")
	}
	// delete: only an own status the store knows
	if r := fx.call(t, sendDelegateRequest{Kind: statusDeleteKind, ID: "SOMEONEELSE1"}); r.OK || f.revokes != 0 {
		t.Fatalf("revoked a status that is not ours: %+v revokes=%d", r, f.revokes)
	}
	r = fx.call(t, sendDelegateRequest{Kind: statusDeleteKind, ID: "STATUSID0001"})
	if !r.OK || f.revokes != 1 {
		t.Fatalf("status delete = %+v revokes=%d", r, f.revokes)
	}
	fx.noRedial(t)
}

func TestActionKindsRefuseWhenDisconnectedAndNeverDial(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	seedOwnMessage(t, fx, fx.app.DB(), actionDM, "TARGET000001", false)
	f.setConnected(false)
	for _, req := range []sendDelegateRequest{
		{Kind: messageStarKind, Chat: actionDM, ID: "TARGET000001", Enable: tr(true)},
		{Kind: contactBlockKind, To: actionDM, SubAction: "block"},
		{Kind: groupPhotoKind, GroupJID: actionGroup, Picture: realJPEG(t)},
		{Kind: privacySetKind, Name: "last", Label: "none"},
		{Kind: chatDeleteKind, Chat: actionDM},
		{Kind: statusPostKind, Message: "x"},
		{Kind: channelCreateKind, Name: "x"},
	} {
		r := fx.call(t, req)
		if r.OK {
			t.Errorf("%s ran while the daemon was disconnected: %+v", req.Kind, r)
		}
	}
	if c, _ := f.calls(); c != 0 {
		t.Fatalf("the daemon dialed %d times from a handler", c)
	}
}

func TestActionKindsAppearInCapabilities(t *testing.T) {
	caps := currentCapabilities()
	have := map[string]bool{}
	for _, k := range caps.Kinds {
		have[k] = true
	}
	for _, k := range []string{messageStarKind, messagePinKind, contactBlockKind, blocklistKind, groupPhotoKind, chatDisappearingKind,
		disappearingDefaultKd, statusPostKind, statusDeleteKind, statusAudienceKind, channelMuteKind, channelCreateKind, chatDeleteKind,
		privacyGetKind, privacySetKind, callRejectKind} {
		if !have[k] {
			t.Errorf("capabilities do not list %s", k)
		}
	}
	if !bytes.Contains([]byte(strings.Join(caps.Features, ",")), []byte("store_pin")) {
		t.Fatalf("features = %v", caps.Features)
	}
}

func init() {
	validatesEmpty[blocklistKind] = true
	validatesEmpty[privacyGetKind] = true
	validatesEmpty[statusAudienceKind] = true
	for _, c := range [][]string{
		{"messages", "star", "--chat", actionDM, "--id", "ABC12345"},
		{"messages", "unstar", "--chat", actionDM, "--id", "ABC12345"},
		{"messages", "pin", "--chat", actionDM, "--id", "ABC12345"},
		{"messages", "unpin", "--chat", actionDM, "--id", "ABC12345"},
		{"messages", "status-delete", "--id", "ABC12345"},
		{"contacts", "block", "--jid", actionDM},
		{"contacts", "unblock", "--jid", actionDM},
		{"contacts", "blocklist"},
		{"groups", "photo", "--jid", actionGroup, "--remove"},
		{"groups", "join-approval", "--jid", actionGroup, "--on"},
		{"groups", "member-add", "--jid", actionGroup, "--off"},
		{"chats", "disappearing", "--chat", actionDM, "--duration", "7d"},
		{"chats", "disappearing-default", "--duration", "off"},
		{"chats", "delete", "--chat", actionDM},
		{"channels", "mute", "--jid", actionChanel},
		{"channels", "unmute", "--jid", actionChanel},
		{"channels", "create", "--name", "Obras"},
		{"privacy", "get"},
		{"messages", "status-audience"},
		{"privacy", "set", "--name", "last", "--value", "contacts"},
		{"calls", "reject", "--jid", actionDM, "--id", "CALLID123456"},
		{"send", "status", "--message", "oi"},
	} {
		registerRefusal(c...)
	}
}

// The CLI form the air gateway runs reaches the daemon with the right kind and fields.
func TestActionCommandsRouteToTheirKind(t *testing.T) {
	cases := []struct {
		args []string
		kind string
		chk  func(sendDelegateRequest) bool
	}{
		{[]string{"messages", "pin", "--chat", actionDM, "--id", "ABC12345", "--duration", "24h", "--new-id", "PINNEWID0001"}, messagePinKind,
			func(r sendDelegateRequest) bool {
				return r.Chat == actionDM && r.ID == "ABC12345" && r.Enable != nil && *r.Enable && r.EphemeralDuration == "24h" && r.NewID == "PINNEWID0001"
			}},
		{[]string{"messages", "unstar", "--chat", actionDM, "--id", "ABC12345"}, messageStarKind,
			func(r sendDelegateRequest) bool { return r.Enable != nil && !*r.Enable }},
		{[]string{"contacts", "block", "--jid", actionDM}, contactBlockKind,
			func(r sendDelegateRequest) bool { return r.To == actionDM && r.SubAction == "block" }},
		{[]string{"groups", "member-add", "--jid", actionGroup, "--on"}, groupSettingKind,
			func(r sendDelegateRequest) bool { return r.SubAction == "member-add" && r.Enable != nil && *r.Enable }},
		{[]string{"chats", "disappearing", "--chat", actionGroup, "--duration", "90d"}, chatDisappearingKind,
			func(r sendDelegateRequest) bool { return r.Chat == actionGroup && r.EphemeralDuration == "90d" }},
		{[]string{"chats", "delete", "--chat", actionDM, "--delete-media"}, chatDeleteKind,
			func(r sendDelegateRequest) bool { return r.Chat == actionDM && r.DeleteMedia }},
		{[]string{"privacy", "set", "--name", "readreceipts", "--value", "none"}, privacySetKind,
			func(r sendDelegateRequest) bool { return r.Name == "readreceipts" && r.Label == "none" }},
		{[]string{"channels", "create", "--name", "Obras", "--about", "x"}, channelCreateKind,
			func(r sendDelegateRequest) bool { return r.Name == "Obras" && r.About == "x" }},
		{[]string{"send", "status", "--message", "oi", "--background-color", "#112233", "--id", "STATUSID0001", "--post-send-wait", "0"}, statusPostKind,
			func(r sendDelegateRequest) bool {
				return r.Message == "oi" && r.BackgroundColor == "#112233" && r.NewID == "STATUSID0001"
			}},
	}
	for _, c := range cases {
		req, stdout := runKindViaSocket(t, `{"ok":true}`, "", c.args...)
		if req.Kind != c.kind || !c.chk(req) {
			t.Errorf("%v: request = %+v, want kind %s", c.args, req, c.kind)
		}
		if !strings.Contains(stdout, `"ok":true`) && !strings.Contains(stdout, `"sent":true`) {
			t.Errorf("%v: stdout = %q", c.args, stdout)
		}
	}
}

// m5: the two app-state writers answer through the "may have gone through" path.
func TestStarAndChatDeleteAreAppStateKinds(t *testing.T) {
	for _, k := range []string{messageStarKind, chatDeleteKind} {
		if !isAppStateKind(k) {
			t.Errorf("%s writes app state and must be in isAppStateKind", k)
		}
	}
}

// m8: the two read kinds run under --read-only; the writes do not.
func TestReadOnlyAllowsOnlyTheReadActionKinds(t *testing.T) {
	flags := &rootFlags{readOnly: true}
	for _, c := range []struct {
		run   func() error
		write bool
	}{
		{func() error {
			return runActionKind(flags, sendDelegateRequest{Kind: chatDeleteKind, Chat: actionDM}, "OK")
		}, true},
		{func() error {
			return runActionKind(flags, sendDelegateRequest{Kind: privacySetKind, Name: "last", Label: "none"}, "OK")
		}, true},
	} {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("write under --read-only: err = %v", err)
		}
	}
	if !readOnlyActionKinds[blocklistKind] || !readOnlyActionKinds[privacyGetKind] || !readOnlyActionKinds[statusAudienceKind] || len(readOnlyActionKinds) != 3 {
		t.Fatalf("readOnlyActionKinds = %v", readOnlyActionKinds)
	}
}

// M1: a relative --file on the command line reaches the daemon as an absolute path.
func TestSendStatusFileGoesToTheDaemonAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "foto.jpg"), realJPEG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	req, _ := runKindViaSocket(t, `{"ok":true}`, "", "send", "status", "--file", "foto.jpg", "--post-send-wait", "0")
	if !filepath.IsAbs(req.File) || filepath.Base(req.File) != "foto.jpg" {
		t.Fatalf("daemon received file %q, want an absolute path to foto.jpg", req.File)
	}
}

// r1: an incoming message with no stored sender cannot be pinned (the key would read as the account's own).
func TestPinRefusesAnIncomingMessageWithoutASender(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	lidChat := "900000001@lid"
	seedOwnMessage(t, fx, fx.app.DB(), lidChat, "INCOMING0001", false)
	r := fx.call(t, sendDelegateRequest{Kind: messagePinKind, Chat: lidChat, ID: "INCOMING0001", Enable: tr(true), EphemeralDuration: "7d"})
	if r.OK || !strings.Contains(r.Error, "no sender") || f.actions.pins != 0 {
		t.Fatalf("pin = %+v pins=%d, want a refusal and no pin on the wire", r, f.actions.pins)
	}
	// Our own message in the same chat still pins.
	seedOwnMessage(t, fx, fx.app.DB(), lidChat, "OWNMESSAGE01", true)
	if r := fx.call(t, sendDelegateRequest{Kind: messagePinKind, Chat: lidChat, ID: "OWNMESSAGE01", Enable: tr(true), EphemeralDuration: "7d"}); !r.OK {
		t.Fatalf("own pin = %+v", r)
	}
}

// r2: the audience read sends nothing, does not dial, and a failed read is an error, not a count of 0.
func TestStatusAudienceIsAReadAndNeverReportsZeroOnError(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	r := fx.call(t, sendDelegateRequest{Kind: statusAudienceKind})
	if !r.OK || !strings.Contains(string(r.Data), `"audience":{"type":"contacts","count":42}`) || len(f.protoSends) != 0 {
		t.Fatalf("audience = %+v data=%s sends=%d", r, r.Data, len(f.protoSends))
	}
	f.mu.Lock()
	f.audienceErr = errors.New("contacts store not available")
	f.mu.Unlock()
	r = fx.call(t, sendDelegateRequest{Kind: statusAudienceKind})
	if r.OK || strings.Contains(string(r.Data), `"count":0`) {
		t.Fatalf("a failed read answered %+v data=%s", r, r.Data)
	}
	// and the post itself still goes out, with no audience claimed
	r = fx.call(t, sendDelegateRequest{Kind: statusPostKind, Message: "x", NewID: "STATUSID0009"})
	if !r.OK || strings.Contains(string(r.Data), "audience") {
		t.Fatalf("post with a failed audience read = %+v data=%s", r, r.Data)
	}
	fx.noRedial(t)
}
