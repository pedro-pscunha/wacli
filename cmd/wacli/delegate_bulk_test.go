package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	registerRefusal("history", "backfill", "--chat", "5511999999999@s.whatsapp.net")
	registerRefusal("media", "backfill")
	registerRefusal("labels", "list")
	validatesEmpty[labelsListKind] = true
	validatesEmpty[mediaBackfillKind] = true
}

func TestBulkKindValidators(t *testing.T) {
	v := func(kind string, r sendDelegateRequest) error { return delegateHandlers[kind].validate(r) }
	ok := sendDelegateRequest{Chat: "5511999999999@s.whatsapp.net", Count: 50, Requests: 3}
	if err := v(historyBackfillKind, ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]sendDelegateRequest{
		"no chat":        {},
		"negative count": {Chat: ok.Chat, Count: -1},
		"count too big":  {Chat: ok.Chat, Count: 501},
		"too many reqs":  {Chat: ok.Chat, Requests: 101},
		"per-chat limit": {Chat: ok.Chat, Count: 500, Requests: 11},
		"negative wait":  {Chat: ok.Chat, WaitMS: -1},
	} {
		if v(historyBackfillKind, bad) == nil {
			t.Errorf("history backfill accepted %s", name)
		}
	}
	if v(historyBackfillKind, sendDelegateRequest{Chat: ok.Chat, Count: 500, Requests: 10}) != nil {
		t.Error("the per-chat limit is inclusive")
	}
	if v(mediaBackfillKind, sendDelegateRequest{Limit: -1}) == nil || v(mediaBackfillKind, sendDelegateRequest{Workers: 17}) == nil || v(mediaBackfillKind, sendDelegateRequest{Chat: "a@b.example"}) == nil {
		t.Error("media backfill accepts bad input")
	}
	if v(mediaBackfillKind, sendDelegateRequest{Chat: "123@g.us", Limit: 10, Workers: 4}) != nil {
		t.Error("media backfill rejects good input")
	}
}

func TestBulkKindsDelegateThroughSocket(t *testing.T) {
	req, stdout := runKindViaSocket(t, `{"chat":"123@g.us","requests_sent":2,"responses_seen":2,"messages_added":80,"messages_synced":80}`, "",
		"history", "backfill", "--chat", "123@g.us", "--count", "40", "--requests", "2", "--wait", "30s", "--idle-exit", "9s")
	if req.Kind != historyBackfillKind || req.Chat != "123@g.us" || req.Count != 40 || req.Requests != 2 || req.WaitMS != 30000 || req.IdleExitMS != 9000 {
		t.Fatalf("req = %+v", req)
	}
	if !strings.Contains(stdout, `"messages_added":80`) {
		t.Fatalf("stdout = %q", stdout)
	}
	req, stdout = runKindViaSocket(t, `{"pending":9,"attempted":5,"downloaded":4,"skipped":0,"failed":1}`, "",
		"media", "backfill", "--chat", "123@g.us", "--limit", "5", "--workers", "2")
	if req.Kind != mediaBackfillKind || req.Limit != 5 || req.Workers != 2 || !strings.Contains(stdout, `"downloaded":4`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
	req, stdout = runKindViaSocket(t, `{"labels":[{"id":"1","name":"Obra","color":3,"deleted":false,"chats":[]}]}`, "", "labels", "list")
	if req.Kind != labelsListKind || !strings.Contains(stdout, `"name":"Obra"`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
}

func TestHistoryBackfillRejectsBadInputBeforeIO(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "history", "backfill", "--chat", "123@g.us", "--count", "501"})
	if err == nil || strings.Contains(err.Error(), "refusing to connect") || !strings.Contains(err.Error(), "--count") {
		t.Fatalf("err = %v", err)
	}
}

// Every long bulk kind has a slot of its own: a media backfill that runs for
// hours must not delay a history repair, and neither touches sends or exact
// media downloads. Two requests of the same kind still queue, and the expiry
// error names the kind.
func TestBulkKindsHaveSeparateSlots(t *testing.T) {
	slots := newDelegateSlots()
	if slots.slotFor(historyBackfillKind) == nil || slots.slotFor(mediaBackfillKind) == nil {
		t.Fatal("a bulk kind has no slot")
	}
	if slots.slotFor(historyBackfillKind) == slots.slotFor(mediaBackfillKind) {
		t.Fatal("history and media backfill share a slot")
	}
	if slots.slotFor(historyBackfillKind) == slots.send || slots.slotFor(mediaBackfillKind) == slots.media {
		t.Fatal("a backfill shares the send or the exact-download slot")
	}
	if slots.slotFor(mediaDownloadKind) != slots.media {
		t.Fatal("exact downloads lost their slot")
	}

	run := func(kind string, taken chan struct{}) (sendDelegateResponse, bool) {
		serverConn, clientConn := net.Pipe()
		defer clientConn.Close()
		ran := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			serveDelegateConn(context.Background(), serverConn, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
				ran <- struct{}{}
				return sendDelegateResponse{OK: true}, nil
			}, slots, newSendPacer(sendSpacing{}))
		}()
		_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
		if err := json.NewEncoder(clientConn).Encode(sendDelegateRequest{Version: sendDelegateVersion, Kind: kind, TimeoutMS: 200}); err != nil {
			t.Fatal(err)
		}
		var resp sendDelegateResponse
		if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		<-done
		return resp, len(ran) == 1
	}

	// A media backfill is running: take its slot. History still runs.
	media := slots.slotFor(mediaBackfillKind)
	<-media
	if resp, ran := run(historyBackfillKind, nil); !resp.OK || !ran {
		t.Fatalf("history backfill waited behind media backfill: %+v ran=%v", resp, ran)
	}
	// A second media backfill queues, then expires, and the error names it.
	resp, ran := run(mediaBackfillKind, nil)
	if resp.OK || ran || !strings.Contains(resp.Error, mediaBackfillKind) {
		t.Fatalf("second media backfill: %+v ran=%v", resp, ran)
	}
	media <- struct{}{}
}

// stop must not return while a request still runs: the caller closes the
// database next. A request that honors cancellation returns at once; one that
// ignores it is abandoned after a bounded wait.
func TestStopWaitsForInFlightHandlers(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	release := make(chan struct{})
	started := make(chan struct{})
	stop, err := startSendDelegateServerForStore(context.Background(), storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		close(started)
		<-release // ignores cancellation
		return sendDelegateResponse{OK: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sendDelegateSocketPath(storeDir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(sendDelegateRequest{Version: sendDelegateVersion, Kind: "text", TimeoutMS: 20000}); err != nil {
		t.Fatal(err)
	}
	<-started

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while a request that ignores cancellation was still running")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after the request finished")
	}
}

// MAJ-A. The tail of a sync is: the sync context ends, Sync removes the store
// handler, the command's deferred stop runs, and only then closeApp disconnects.
// whatsmeow acks an inbound message whether or not a handler is registered, so
// the time between "handler removed" and "disconnected" is a window in which a
// message is acked and dropped. A request in flight must not hold that window
// open: it is cancelled the moment the sync ends. This replays the order with
// the real server and the real stop, a request that honors cancellation, and an
// inbound message delivered while the window is open.
func TestStopDoesNotKeepTheConnectionUpWithoutAStoreHandler(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	syncCtx, endSync := context.WithCancel(context.Background())
	started := make(chan struct{})
	stop, err := startSendDelegateServerForStore(syncCtx, storeDir, sendSpacing{}, func(ctx context.Context, req sendDelegateRequest) (sendDelegateResponse, error) {
		close(started)
		<-ctx.Done() // a send waiting for its ack: it returns when its context ends
		return sendDelegateResponse{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", sendDelegateSocketPath(storeDir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(sendDelegateRequest{Version: sendDelegateVersion, Kind: "text", TimeoutMS: 60000}); err != nil {
		t.Fatal(err)
	}
	<-started

	// The daemon's connection: stored while the handler is registered, acked and
	// dropped while it is up with no handler.
	var mu sync.Mutex
	handlerRegistered, connected := true, true
	var stored, dropped int
	deliver := func() {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case !connected:
		case handlerRegistered:
			stored++
		default:
			dropped++
		}
	}
	stopDeliveries := make(chan struct{})
	deliveriesDone := make(chan struct{})
	go func() {
		defer close(deliveriesDone)
		for {
			select {
			case <-stopDeliveries:
				return
			case <-time.After(2 * time.Millisecond):
				deliver()
			}
		}
	}()

	// The sync ends: context cancelled, handler removed, stop, disconnect.
	endSync()
	mu.Lock()
	handlerRegistered = false
	windowStart := time.Now()
	mu.Unlock()
	stop()
	mu.Lock()
	connected = false
	window := time.Since(windowStart)
	mu.Unlock()
	close(stopDeliveries)
	<-deliveriesDone

	if window > 500*time.Millisecond {
		t.Fatalf("the connection stayed up %v with no store handler; each 2 ms of it can drop a message", window)
	}
	mu.Lock()
	defer mu.Unlock()
	// At 2 ms per delivery a window under 500 ms is at most a handful of
	// messages, and these arrive only while stop is returning, not for the
	// seconds an in-flight upload used to hold it open.
	if dropped > int(window/(2*time.Millisecond))+2 {
		t.Fatalf("dropped %d messages (stored %d) in a %v window", dropped, stored, window)
	}
}
