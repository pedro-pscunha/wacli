package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func exactRetryFixture(t *testing.T) (*App, *fakeWA, string) {
	t.Helper()
	a := newTestApp(t)
	f := newFakeWA()
	a.wa = f
	chat := "111@s.whatsapp.net"
	if err := a.db.UpsertChat(chat, "dm", "Synthetic", time.Now()); err != nil {
		t.Fatal(err)
	}
	insertMediaMessage(t, a, chat, "target", time.Now().Add(-time.Hour))
	return a, f, chat
}

func TestRetryMediaMessageSelectsExactIdentityDespiteLocalPath(t *testing.T) {
	a, f, chat := exactRetryFixture(t)
	insertMediaMessage(t, a, chat, "newer", time.Now())
	other := "222@s.whatsapp.net"
	if err := a.db.UpsertChat(other, "dm", "Other", time.Now()); err != nil {
		t.Fatal(err)
	}
	insertMediaMessage(t, a, other, "target", time.Now())
	// A stale local path removes this row from batch queries but must not hide
	// the explicitly requested message from phone recovery.
	if err := a.db.MarkMediaDownloaded(chat, "target", filepath.Join(t.TempDir(), "missing"), time.Now()); err != nil {
		t.Fatal(err)
	}
	f.onMediaRetry = func(info *types.MessageInfo, key []byte) any {
		if info.Chat.String() != chat || info.ID != "target" {
			t.Fatalf("wrong receipt: %+v", info)
		}
		return notOnPhoneHook(info, key)
	}
	out, err := a.RetryMediaMessage(context.Background(), chat, "target", time.Millisecond)
	if err != nil || out.Status != "recovered" || out.ChatJID != chat || out.MsgID != "target" || len(f.mediaRetryReceipts) != 1 {
		t.Fatalf("outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
	}
	for _, target := range [][2]string{{chat, "newer"}, {other, "target"}} {
		info, err := a.db.GetMediaDownloadInfo(target[0], target[1])
		if err != nil || info.LocalPath != "" {
			t.Fatalf("unrelated message changed: %+v error=%v", info, err)
		}
	}
	// Existing corrupt bytes are also irrelevant to exact phone selection.
	corrupt := filepath.Join(t.TempDir(), "corrupt")
	if err := os.WriteFile(corrupt, []byte("invalid media bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.db.MarkMediaDownloaded(chat, "target", corrupt, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err = a.RetryMediaMessage(context.Background(), chat, "target", time.Millisecond)
	if err != nil || out.Status != "recovered" || len(f.mediaRetryReceipts) != 2 {
		t.Fatalf("corrupt cache selection outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
	}
}

func TestRetryMediaMessageIgnoresUnrelatedNotificationsAndSendsOnce(t *testing.T) {
	for _, wrong := range []string{"chat", "id", "none"} {
		t.Run(wrong, func(t *testing.T) {
			a, f, chat := exactRetryFixture(t)
			f.onMediaRetry = func(info *types.MessageInfo, _ []byte) any {
				if wrong == "none" {
					return nil
				}
				evt := &events.MediaRetry{ChatID: info.Chat, MessageID: info.ID, Error: &events.MediaRetryError{Code: 2}}
				if wrong == "chat" {
					evt.ChatID = types.NewJID("222", types.DefaultUserServer)
				} else {
					evt.MessageID = "other"
				}
				return evt
			}
			out, err := a.RetryMediaMessage(context.Background(), chat, "target", 10*time.Millisecond)
			if err != nil || out.Status != "no_response" || len(f.mediaRetryReceipts) != 1 {
				t.Fatalf("outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
			}
		})
	}
}

func TestRetryMediaMessageAcceptsMatchingResponseAfterUnrelatedEvents(t *testing.T) {
	a, f, chat := exactRetryFixture(t)
	f.downloadErr = whatsmeow.ErrMediaDownloadFailedWith404
	f.onMediaRetry = func(info *types.MessageInfo, key []byte) any {
		// Earlier notifications for either half of the identity must not consume
		// the matching response or cause a download for an unrelated message.
		f.emit(&events.MediaRetry{ChatID: types.NewJID("222", types.DefaultUserServer), MessageID: info.ID, Error: &events.MediaRetryError{Code: 9}})
		f.emit(&events.MediaRetry{ChatID: info.Chat, MessageID: "other", Error: &events.MediaRetryError{Code: 9}})
		return notOnPhoneHook(info, key)
	}
	out, err := a.RetryMediaMessage(context.Background(), chat, "target", time.Millisecond)
	if err != nil || out.Status != "not_on_phone" || len(f.mediaRetryReceipts) != 1 {
		t.Fatalf("correlated outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
	}
}

func TestRetryMediaMessagePhoneGoneRequiresExpiredCDN(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
	}{
		{"expired", "not_on_phone", whatsmeow.ErrMediaDownloadFailedWith410},
		{"transient", "error", errors.New("temporary network failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, chat := exactRetryFixture(t)
			f.onMediaRetry, f.downloadErr = notOnPhoneHook, tc.err
			out, err := a.RetryMediaMessage(context.Background(), chat, "target", time.Millisecond)
			if err != nil || out.Status != tc.status {
				t.Fatalf("outcome=%+v error=%v", out, err)
			}
			n, err := a.db.CountPendingMediaDownloads(context.Background(), chat)
			want := 1
			if tc.status == "not_on_phone" {
				want = 0
			}
			if err != nil || n != want {
				t.Fatalf("pending=%d want=%d error=%v", n, want, err)
			}
		})
	}
}

// Capture the effective deadline without waiting thirty seconds in the test.
type exactDeadlineWA struct {
	*fakeWA
	deadline time.Time
}

func (f *exactDeadlineWA) SendMediaRetryReceipt(ctx context.Context, info *types.MessageInfo, key []byte) error {
	f.deadline, _ = ctx.Deadline()
	return f.fakeWA.SendMediaRetryReceipt(ctx, info, key)
}

func TestRetryMediaMessageWaitCapAndCallerDeadline(t *testing.T) {
	a, base, chat := exactRetryFixture(t)
	f := &exactDeadlineWA{fakeWA: base}
	a.wa = f
	base.onMediaRetry = notOnPhoneHook
	start := time.Now()
	if _, err := a.RetryMediaMessage(context.Background(), chat, "target", time.Hour); err != nil {
		t.Fatal(err)
	}
	if f.deadline.IsZero() || f.deadline.After(start.Add(30*time.Second+100*time.Millisecond)) || f.deadline.Before(start.Add(29*time.Second)) {
		t.Fatalf("uncapped deadline: %v", f.deadline)
	}
	base.onMediaRetry = nil
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	start = time.Now()
	_, err := a.RetryMediaMessage(ctx, chat, "target", time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || !f.deadline.Equal(want) || time.Since(start) > time.Second {
		t.Fatalf("caller deadline lost: deadline=%v want=%v error=%v", f.deadline, want, err)
	}
}

func TestRetryMediaMessageRejectsMissingSelector(t *testing.T) {
	a, f, chat := exactRetryFixture(t)
	for _, pair := range [][2]string{{"", "target"}, {chat, " "}, {" ", "target"}} {
		if _, err := a.RetryMediaMessage(context.Background(), pair[0], pair[1], time.Millisecond); err == nil {
			t.Fatalf("accepted selector %v", pair)
		}
	}
	if _, err := a.RetryMediaMessage(context.Background(), chat, "target", -time.Second); err == nil {
		t.Fatal("accepted negative wait")
	}
	if len(f.mediaRetryReceipts) != 0 {
		t.Fatalf("invalid input sent receipts: %v", f.mediaRetryReceipts)
	}
	out, err := a.RetryMediaMessage(context.Background(), chat, "missing", time.Millisecond)
	if err != nil || out.Status != "error" || len(f.mediaRetryReceipts) != 0 {
		t.Fatalf("missing message outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
	}
}

// The phone's answer carries a fresh direct path, so a stored row without one still sends its receipt (a silent
// phone is then a no_response, not a refusal). When the
// phone has no copy there is no server copy to try either: the row is gone, and the server is never asked.
func TestRetryMediaMessageWithoutDirectPath(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		hook         func(*types.MessageInfo, []byte) any
	}{
		{"phone-silent", "no_response", nil},
		{"phone-gone", "not_on_phone", notOnPhoneHook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			chat := "111@s.whatsapp.net"
			if err := a.db.UpsertChat(chat, "dm", "Synthetic", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := a.db.UpsertMessage(store.UpsertMessageParams{ChatJID: chat, MsgID: "old", SenderJID: chat, FromMe: true,
				Timestamp: time.Now().Add(-time.Hour), MediaType: "image", MimeType: "image/jpeg", MediaKey: make([]byte, 32),
				FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32), FileLength: 3}); err != nil {
				t.Fatal(err)
			}
			f.onMediaRetry = tc.hook
			f.downloadErr = errors.New("the server must not be asked")
			out, err := a.RetryMediaMessage(context.Background(), chat, "old", time.Millisecond)
			if err != nil || out.Status != tc.status || len(f.mediaRetryReceipts) != 1 {
				t.Fatalf("outcome=%+v error=%v receipts=%v", out, err, f.mediaRetryReceipts)
			}
			if marked := mediaUnavailable(t, a, chat, "old"); marked != (tc.status == "not_on_phone") {
				t.Fatalf("media_unavailable_at set = %v for %s", marked, tc.status)
			}
		})
	}
}

// Without a direct path the phone is the only source, and its answer is useless without the full key and hash.
func TestRetryMediaMessageWithoutDirectPathNeedsTheFullKeyAndHash(t *testing.T) {
	for name, row := range map[string]store.UpsertMessageParams{
		"short-key": {MediaKey: make([]byte, 16), FileSHA256: make([]byte, 32)},
		"no-sha":    {MediaKey: make([]byte, 32)},
		"short-sha": {MediaKey: make([]byte, 32), FileSHA256: []byte("sha")},
	} {
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t)
			f := newFakeWA()
			a.wa = f
			chat := "111@s.whatsapp.net"
			if err := a.db.UpsertChat(chat, "dm", "Synthetic", time.Now()); err != nil {
				t.Fatal(err)
			}
			row.ChatJID, row.MsgID, row.SenderJID, row.Timestamp, row.MediaType = chat, "old", chat, time.Now(), "image"
			if err := a.db.UpsertMessage(row); err != nil {
				t.Fatal(err)
			}
			out, _ := a.RetryMediaMessage(context.Background(), chat, "old", time.Millisecond)
			if out.Status != "error" || len(f.mediaRetryReceipts) != 0 {
				t.Fatalf("outcome=%+v receipts=%v", out, f.mediaRetryReceipts)
			}
		})
	}
}

func mediaUnavailable(t *testing.T, a *App, chat, id string) bool {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(a.opts.StoreDir, "wacli.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var at sql.NullInt64
	if err := db.QueryRow(`SELECT media_unavailable_at FROM messages WHERE chat_jid = ? AND msg_id = ?`, chat, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.Valid
}
