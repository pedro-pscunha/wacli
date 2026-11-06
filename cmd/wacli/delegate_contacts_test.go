package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/config"
)

const contactJID = "5511999999999@s.whatsapp.net"

func init() {
	registerRefusalLocalOK("contacts", "alias", "set", "--jid", contactJID, "--alias", "Obra A")
	registerRefusalLocalOK("contacts", "alias", "rm", "--jid", contactJID)
	registerRefusalLocalOK("contacts", "tags", "add", "--jid", contactJID, "--tag", "obra")
	registerRefusalLocalOK("contacts", "tags", "rm", "--jid", contactJID, "--tag", "obra")
}

// The contact kinds never connect, so with no daemon they run locally (even on a pinned store) and with
// --delegate-only they are NOT refused: that flag is about connections. The shared refusal list is for
// commands that connect, so these are recorded apart.
var localOKCommands [][]string

func registerRefusalLocalOK(args ...string) { localOKCommands = append(localOKCommands, args) }

func TestContactKindValidators(t *testing.T) {
	v := func(kind string, r sendDelegateRequest) error { return delegateHandlers[kind].validate(r) }
	ok := sendDelegateRequest{To: contactJID, SubAction: "set", Alias: "Obra A"}
	if err := v(contactAliasKind, ok); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", maxContactLabelRunes+1)
	for name, bad := range map[string]sendDelegateRequest{
		"no jid":         {SubAction: "set", Alias: "a"},
		"blank alias":    {To: contactJID, SubAction: "set", Alias: "  "},
		"long alias":     {To: contactJID, SubAction: "set", Alias: long},
		"control char":   {To: contactJID, SubAction: "set", Alias: "a\x00b"},
		"newline":        {To: contactJID, SubAction: "set", Alias: "a\nb"},
		"unknown action": {To: contactJID, SubAction: "wipe"},
		"alias on rm":    {To: contactJID, SubAction: "rm", Alias: "x"},
	} {
		if v(contactAliasKind, bad) == nil {
			t.Errorf("alias accepted %s", name)
		}
	}
	if v(contactAliasKind, sendDelegateRequest{To: contactJID, SubAction: "rm"}) != nil {
		t.Error("alias rm needs only the jid")
	}
	if v(contactTagKind, sendDelegateRequest{To: contactJID, SubAction: "add", Tag: "obra"}) != nil ||
		v(contactTagKind, sendDelegateRequest{To: contactJID, SubAction: "rm", Tag: "obra"}) != nil {
		t.Error("a plain tag add/rm is valid")
	}
	for name, bad := range map[string]sendDelegateRequest{
		"no tag":         {To: contactJID, SubAction: "add"},
		"long tag":       {To: contactJID, SubAction: "add", Tag: long},
		"bad action":     {To: contactJID, SubAction: "set", Tag: "t"},
		"control char":   {To: contactJID, SubAction: "add", Tag: "t\tb"},
		"no jid":         {SubAction: "add", Tag: "t"},
		"rm without tag": {To: contactJID, SubAction: "rm"},
	} {
		if v(contactTagKind, bad) == nil {
			t.Errorf("tag accepted %s", name)
		}
	}
}

func TestContactKindsAreLocalAndSkipTheSendQueue(t *testing.T) {
	for _, k := range []string{contactAliasKind, contactTagKind} {
		h := delegateHandlers[k]
		if !h.skipsQueue || !h.localOK {
			t.Errorf("%s: skipsQueue=%v localOK=%v, want both", k, h.skipsQueue, h.localOK)
		}
	}
}

// The daemon side, with the real server and executor and a fake WhatsApp client: storage is asserted and
// the connection is never touched.
func TestDaemonRunsContactAliasAndTag(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	db := fx.app.DB()
	if err := db.UpsertContact(contactJID, "5511999999999", "", "Fulano", "", ""); err != nil {
		t.Fatal(err)
	}

	if r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: contactJID, Alias: "  Obra A  ", SubAction: "set"}); !r.OK {
		t.Fatalf("alias set = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: contactTagKind, To: contactJID, Tag: "obra", SubAction: "add"}); !r.OK {
		t.Fatalf("tag add = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: contactTagKind, To: contactJID, Tag: "fornecedor", SubAction: "add"}); !r.OK {
		t.Fatalf("tag add 2 = %+v", r)
	}
	c, err := db.GetContact(contactJID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alias != "Obra A" || len(c.Tags) != 2 {
		t.Fatalf("stored contact = %+v, want alias trimmed and two tags", c)
	}

	if r := fx.call(t, sendDelegateRequest{Kind: contactTagKind, To: contactJID, Tag: "obra", SubAction: "rm"}); !r.OK {
		t.Fatalf("tag rm = %+v", r)
	}
	if r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: contactJID, SubAction: "rm"}); !r.OK {
		t.Fatalf("alias rm = %+v", r)
	}
	c, _ = db.GetContact(contactJID)
	if c.Alias != "" || len(c.Tags) != 1 || c.Tags[0] != "fornecedor" {
		t.Fatalf("after removals = %+v", c)
	}

	// The daemon validates again: a bad request is refused before it touches the store.
	if r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: contactJID, SubAction: "set", Alias: "a\x00"}); r.OK {
		t.Fatalf("a control character was accepted: %+v", r)
	}
	fx.noRedial(t)
}

// With no daemon, on a pinned store, the commands run locally under the lock: they never connect, so the pin
// does not block them, and --delegate-only does not either.
func TestContactCommandsWorkLocallyOnAPinnedStoreWithoutDaemon(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	store := t.TempDir()
	db := openSystemImportStore(t, store)
	if err := db.UpsertContact(contactJID, "5511999999999", "", "Fulano", "", ""); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.WriteFile(filepath.Join(store, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range localOKCommands {
		if err := execute(append([]string{"--store", store, "--delegate-only", "--json"}, c...)); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	// ...and the last command in the list removed the tag it added; the alias set then rm left nothing.
	db2 := openSystemImportStore(t, store)
	defer db2.Close()
	c, err := db2.GetContact(contactJID)
	if err != nil || c.Alias != "" || len(c.Tags) != 0 {
		t.Fatalf("contact after set/rm, add/rm = %+v err=%v", c, err)
	}
}

// With a daemon listening, the CLI sends the request to it and does not touch the store itself.
func TestContactCommandsRouteToTheDaemonWhenItListens(t *testing.T) {
	req, stdout := runKindViaSocket(t, `{"jid":"`+contactJID+`","alias":"Obra A"}`, "",
		"contacts", "alias", "set", "--jid", contactJID, "--alias", "Obra A")
	if req.Kind != contactAliasKind || req.To != contactJID || req.Alias != "Obra A" || req.SubAction != "set" || !strings.Contains(stdout, `"alias":"Obra A"`) {
		t.Fatalf("alias set req=%+v stdout=%q", req, stdout)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+contactJID+`","removed":true}`, "", "contacts", "alias", "rm", "--jid", contactJID)
	if req.Kind != contactAliasKind || req.SubAction != "rm" || req.Alias != "" {
		t.Fatalf("alias rm req=%+v", req)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+contactJID+`","tag":"obra"}`, "", "contacts", "tags", "add", "--jid", contactJID, "--tag", "obra")
	if req.Kind != contactTagKind || req.SubAction != "add" || req.Tag != "obra" {
		t.Fatalf("tag add req=%+v", req)
	}
	req, _ = runKindViaSocket(t, `{"jid":"`+contactJID+`","tag":"obra","removed":true}`, "", "contacts", "tags", "rm", "--jid", contactJID, "--tag", "obra")
	if req.Kind != contactTagKind || req.SubAction != "rm" || req.Tag != "obra" {
		t.Fatalf("tag rm req=%+v", req)
	}
}

func TestContactCommandsRejectBadInputBeforeAnyIO(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "contacts", "alias", "set", "--jid", contactJID, "--alias", strings.Repeat("x", 101)})
	if err == nil || !strings.Contains(err.Error(), "the limit is 100") {
		t.Fatalf("err = %v", err)
	}
}

func TestContactKindsAppearInCapabilities(t *testing.T) {
	have := map[string]bool{}
	for _, k := range currentCapabilities().Kinds {
		have[k] = true
	}
	if !have[contactAliasKind] || !have[contactTagKind] {
		t.Fatalf("capabilities lack the contact kinds: %v", currentCapabilities().Kinds)
	}
}
