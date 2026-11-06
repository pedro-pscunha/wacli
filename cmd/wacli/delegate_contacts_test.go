package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
)

const contactJID = "5511999999999@s.whatsapp.net"

func init() {
	registerLocalOK("contacts", "alias", "set", "--jid", contactJID, "--alias", "Obra A")
	registerLocalOK("contacts", "alias", "rm", "--jid", contactJID)
	registerLocalOK("contacts", "tags", "add", "--jid", contactJID, "--tag", "obra")
	registerLocalOK("contacts", "tags", "rm", "--jid", contactJID, "--tag", "obra")
}

// The contact kinds never connect, so with no daemon they run locally (even on a pinned store) and with
// --delegate-only they are NOT refused: that flag is about connections. The shared refusal list is for
// commands that connect, so these are recorded apart.
var localOKCommands [][]string

func registerLocalOK(args ...string) { localOKCommands = append(localOKCommands, args) }

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

// ---- review of 05deb5a ---------------------------------------------------------------------------------------

// M1. The daemon resolves LID/PN through its live client, not through a read-only handle on session.db that
// is opened immutable=1 and kept for the daemon's life. A real session.db is on disk with NO mapping for the
// contact; the live client learns one AFTER the first call. The second call must write both identities. With the
// old code the cached read-only handle never sees the mapping (and the file has none at all), so only one
// identity is written.
func TestDaemonResolvesContactIdentitiesThroughTheLiveClient(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	session, err := sql.Open("sqlite3", filepath.Join(fx.dir, "session.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	session.Close()
	db := fx.app.DB()
	const pn, lid = "5511999999999", "900000001"
	for _, j := range []string{pn + "@s.whatsapp.net", lid + "@lid"} {
		if err := db.UpsertContact(j, pn, "", "Fulano", "", ""); err != nil {
			t.Fatal(err)
		}
	}

	// First call, before any mapping exists: only the phone identity can be written.
	if r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: pn + "@s.whatsapp.net", Alias: "Um", SubAction: "set"}); !r.OK {
		t.Fatalf("first call = %+v", r)
	}
	if c, _ := db.GetContact(lid + "@lid"); c.Alias != "" {
		t.Fatalf("the LID identity got an alias without a mapping: %+v", c)
	}

	// whatsmeow learns the mapping while the daemon runs. The file on disk still has none.
	f.learnLID(lid, pn)

	if r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: pn + "@s.whatsapp.net", Alias: "Dois", SubAction: "set"}); !r.OK {
		t.Fatalf("second call = %+v", r)
	}
	for _, j := range []string{pn + "@s.whatsapp.net", lid + "@lid"} {
		c, err := db.GetContact(j)
		if err != nil || c.Alias != "Dois" {
			t.Fatalf("%s after the mapping was learned = %+v err=%v, want alias Dois on both identities", j, c, err)
		}
	}
	// A LID typed by the caller maps back to the phone number through the live client too.
	if r := fx.call(t, sendDelegateRequest{Kind: contactTagKind, To: lid + "@lid", Tag: "obra", SubAction: "add"}); !r.OK {
		t.Fatalf("tag by LID = %+v", r)
	}
	if c, _ := db.GetContact(pn + "@s.whatsapp.net"); len(c.Tags) != 1 || c.Tags[0] != "obra" {
		t.Fatalf("tag typed as a LID did not reach the phone identity: %+v", c)
	}
	fx.noRedial(t)
}

// m3. Format, separator and control characters, and invalid UTF-8, are refused.
func TestContactLabelRejectsFormatSeparatorAndInvalidUTF8(t *testing.T) {
	for name, label := range map[string]string{
		"bidi override U+202E": "abc\u202edef",
		"zero width U+200B":    "a\u200bb",
		"BOM U+FEFF":           "\ufeffabc",
		"line sep U+2028":      "a\u2028b",
		"para sep U+2029":      "a\u2029b",
		"NUL":                  "a\x00b",
		"tab":                  "a\tb",
		"invalid utf-8":        "a\xffb",
		"invalid utf-8 lone":   "\xc3\x28",
	} {
		if validateContactLabel("--alias", label) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, ok := range []string{"Obra A", "Concreto & Cia", "São José", "fornecedor 2", "日本語", "a b"} {
		if err := validateContactLabel("--alias", ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
}

// m4. A bare number is normalized to a JID and the stored key is echoed; it is never stored under the raw input.
func TestBareNumberIsStoredUnderTheCanonicalJID(t *testing.T) {
	f := newDaemonFakeWA()
	fx := newDaemonFixture(t, f)
	db := fx.app.DB()
	if err := db.UpsertContact("5511999999999@s.whatsapp.net", "5511999999999", "", "Fulano", "", ""); err != nil {
		t.Fatal(err)
	}
	r := fx.call(t, sendDelegateRequest{Kind: contactAliasKind, To: "+55 11 99999-9999", Alias: "Obra", SubAction: "set"})
	if !r.OK || !strings.Contains(string(r.Data), `"jid":"5511999999999@s.whatsapp.net"`) {
		t.Fatalf("answer = %+v data=%s, want the canonical JID echoed", r, r.Data)
	}
	if c, _ := db.GetContact("5511999999999@s.whatsapp.net"); c.Alias != "Obra" {
		t.Fatalf("the contact row has no alias: %+v", c)
	}
	if c, err := db.GetContact("5511999999999"); err == nil && c.Alias != "" {
		t.Fatalf("metadata was stored under the raw number: %+v", c)
	}
}

// m5. The CLI prints the stored (trimmed) value, and refuses --alias on rm.
func TestContactAliasPrintsTheStoredValueAndRefusesAliasOnRm(t *testing.T) {
	_, stdout := runKindViaSocket(t, `{"jid":"`+contactJID+`","alias":"Obra A"}`, "",
		"contacts", "alias", "set", "--jid", contactJID, "--alias", "   Obra A   ")
	if !strings.Contains(stdout, `"alias":"Obra A"`) || strings.Contains(stdout, "   ") {
		t.Fatalf("the CLI printed the raw input: %q", stdout)
	}
	err := execute([]string{"--store", t.TempDir(), "contacts", "alias", "rm", "--jid", contactJID, "--alias", "X"})
	if err == nil || !strings.Contains(err.Error(), "does not apply to rm") {
		t.Fatalf("alias rm --alias X: err = %v, want a refusal", err)
	}
}

// m7. The set of kinds that may run without a connection is a security boundary: pin it.
func TestLocalOKKindsAreExactly(t *testing.T) {
	// Adding a kind here means it never touches the WhatsApp connection or the session store, and that someone
	// reviewed it. A kind that connects with localOK set would run on an unpinned store without --delegate-only.
	want := map[string]bool{contactAliasKind: true, contactTagKind: true}
	got := map[string]bool{}
	for k, h := range delegateHandlers {
		if h.localOK {
			got[k] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("localOK kinds = %v, want exactly %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("localOK kinds = %v, want exactly %v", got, want)
		}
	}
}

// m7. The App a localOK kind runs on cannot connect or open the session, on ANY store.
func TestKindLocalAppCannotConnectOrOpenTheSession(t *testing.T) {
	isolateAccountConfigHome(t)
	t.Setenv("WACLI_DELEGATE_ONLY", "")
	for name, pinned := range map[string]bool{"unpinned": false, "pinned": true} {
		t.Run(name, func(t *testing.T) {
			store := t.TempDir()
			if pinned {
				if err := os.WriteFile(filepath.Join(store, config.DelegateOnlyMarker), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a, lk, err := newKindLocalApp(context.Background(), &rootFlags{storeDir: store})
			if err != nil {
				t.Fatal(err)
			}
			defer closeApp(a, lk)
			if err := a.Connect(context.Background(), false, nil); !errors.Is(err, app.ErrDelegateOnly) {
				t.Errorf("Connect err = %v", err)
			}
			if err := a.EnsureAuthed(context.Background()); !errors.Is(err, app.ErrDelegateOnly) {
				t.Errorf("EnsureAuthed err = %v", err)
			}
			if err := a.OpenWA(); !errors.Is(err, app.ErrNoClient) {
				t.Errorf("OpenWA err = %v, want ErrNoClient", err)
			}
			if _, err := a.LocalResolver(); err == nil {
				t.Error("LocalResolver built a client")
			}
			if _, statErr := os.Stat(filepath.Join(store, "session.db")); statErr == nil {
				t.Error("session.db was created")
			}
		})
	}
}

// m7. A pinned store with a daemon listening: the call is served by the daemon, and the store is not opened.
func TestContactCommandOnAPinnedStoreRoutesToTheListeningDaemon(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	if err := os.WriteFile(filepath.Join(storeDir, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()
	server := startPresenceDelegateTestSocket(t, storeDir, func(sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Data: []byte(`{"jid":"` + contactJID + `","tag":"obra"}`)}
	})
	defer server.stop()
	stdout, stderr, runErr := runPresenceDelegateHelper(t, []string{"--store", storeDir, "--json", "--timeout", "750ms", "contacts", "tags", "add", "--jid", contactJID, "--tag", "obra"})
	if runErr != nil {
		t.Fatalf("pinned + daemon listening failed: %v stdout=%q stderr=%q", runErr, stdout, stderr)
	}
	if req := server.nextRequest(t); req.Kind != contactTagKind {
		t.Fatalf("req = %+v", req)
	}
	if _, statErr := os.Stat(filepath.Join(storeDir, "wacli.db")); statErr == nil {
		t.Error("the CLI opened wacli.db although the daemon serves the store")
	}
}

// m1. The daemon bound its socket between the CLI's dial and its lock attempt. The lock is held, so the call
// is delegated; it must never answer "stop the daemon to run local maintenance" (which the gateway reads as
// "this daemon cannot do it").
func TestLocalOKCallDelegatesWhenTheDaemonStartsBetweenTheDialAndTheLock(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	if err := os.WriteFile(filepath.Join(storeDir, config.DelegateOnlyMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// What the race leaves behind: the first dial found nothing, then the socket appeared. openApp's own dial
	// sees it and answers errPinnedStoreMaintenance; callKind must turn that into a delegation.
	server := startPresenceDelegateTestSocket(t, storeDir, func(sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Data: []byte(`{"jid":"` + contactJID + `","alias":"Obra"}`)}
	})
	defer server.stop()
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	orig := kindDaemonListening
	kindDaemonListening = func(context.Context, string) bool { return false } // the first dial finds nothing
	defer func() { kindDaemonListening = orig }()

	flags := &rootFlags{storeDir: storeDir, timeout: 2 * time.Second}
	res, err := callKind[contactAckResult](context.Background(), flags, sendDelegateRequest{Kind: contactAliasKind, To: contactJID, Alias: "Obra", SubAction: "set"})
	if err != nil {
		t.Fatalf("err = %v, want the call delegated to the daemon", err)
	}
	if res.Alias != "Obra" {
		t.Fatalf("res = %+v", res)
	}
}
