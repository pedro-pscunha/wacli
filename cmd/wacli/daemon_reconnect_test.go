package main

import (
	"context"

	"github.com/openclaw/wacli/internal/app"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
)

// M1: a retryable send error inside the daemon must never redial. The daemon's
// own reconnect loop owns the socket.
func TestDaemonSendRetryNeverReconnects(t *testing.T) {
	fastDaemonPolling(t)
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{whatsmeow.ErrIQTimedOut}
	a := newDaemonApp(t, f)

	// The daemon's sync loop brings the connection back while the send waits.
	go func() { time.Sleep(30 * time.Millisecond); f.setConnected(true) }()
	f.setConnected(false) // the IQ timeout coincided with a dropped socket

	resp, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{
		Version: sendDelegateVersion, Kind: "text", To: "5511999999999@s.whatsapp.net", Message: "hi", AllowSelf: true, TimeoutMS: 5000,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !resp.Sent || resp.ID == "" {
		t.Fatalf("resp = %+v", resp)
	}
	if c, d := f.calls(); c != 0 || d != 0 {
		t.Fatalf("daemon redialed: connectCalls=%d disconnects=%d", c, d)
	}
	if len(f.sendTexts) != 1 {
		t.Fatalf("sent %d times", len(f.sendTexts))
	}
}

func TestDaemonSendRetryGivesUpWhenDaemonStaysDown(t *testing.T) {
	fastDaemonPolling(t)
	f := newDaemonFakeWA()
	f.sendTextErrs = []error{whatsmeow.ErrIQTimedOut}
	a := newDaemonApp(t, f)
	f.setConnected(false)

	_, err := executeDelegatedSend(context.Background(), a, sendDelegateRequest{
		Version: sendDelegateVersion, Kind: "text", To: "5511999999999@s.whatsapp.net", Message: "hi", AllowSelf: true, TimeoutMS: 300,
	})
	if err == nil || !strings.Contains(err.Error(), "daemon not connected, not sent") {
		t.Fatalf("err = %v", err)
	}
	if c, d := f.calls(); c != 0 || d != 0 {
		t.Fatalf("daemon redialed: connectCalls=%d disconnects=%d", c, d)
	}
	if len(f.sendTexts) != 0 {
		t.Fatalf("a message was sent: %v", f.sendTexts)
	}
}

type reconnectProbe struct {
	f          *daemonFakeWA
	connectErr error
}

func (p *reconnectProbe) WA() app.WAClient { return p.f }
func (p *reconnectProbe) Connect(context.Context, bool, func(string)) error {
	p.f.mu.Lock()
	p.f.connectCalls++
	p.f.mu.Unlock()
	return p.connectErr
}

// Outside the daemon the old behavior stays: drop the socket and dial again.
func TestStandaloneSendRetryStillReconnects(t *testing.T) {
	p := &reconnectProbe{f: newDaemonFakeWA()}
	if err := reconnectForSend(p)(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, d := p.f.calls(); c != 1 || d != 1 {
		t.Fatalf("standalone reconnect: connectCalls=%d disconnects=%d, want 1 and 1", c, d)
	}
}

func TestDaemonReconnectStepNeverDisconnects(t *testing.T) {
	fastDaemonPolling(t)
	p := &reconnectProbe{f: newDaemonFakeWA()}
	ctx := context.WithValue(context.Background(), daemonConnKey{}, true)
	if err := reconnectForSend(p)(ctx); err != nil {
		t.Fatal(err)
	}
	if c, d := p.f.calls(); c != 0 || d != 0 {
		t.Fatalf("daemon reconnect step touched the socket: connectCalls=%d disconnects=%d", c, d)
	}
}
