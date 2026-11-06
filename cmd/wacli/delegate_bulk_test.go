package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
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

// A backfill holds the bulk slot for minutes. A media download and a send must
// not wait for it, and two backfills must not run at once.
func TestBulkKindUsesItsOwnSlot(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	sendSlot := make(chan struct{}, 1)
	sendSlot <- struct{}{}
	mediaSlot := make(chan struct{}, 1)
	mediaSlot <- struct{}{}
	bulkSlot := make(chan struct{}, 1) // empty: another backfill runs
	ran := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleSendDelegateConn(context.Background(), serverConn, func(context.Context, sendDelegateRequest) (sendDelegateResponse, error) {
			ran <- struct{}{}
			return sendDelegateResponse{OK: true}, nil
		}, sendSlot, newSendPacer(sendSpacing{}), mediaSlot, bulkSlot)
	}()
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	enc := sendDelegateRequest{Version: sendDelegateVersion, Kind: mediaBackfillKind, TimeoutMS: 200}
	if err := jsonEncode(clientConn, enc); err != nil {
		t.Fatal(err)
	}
	var resp sendDelegateResponse
	if err := jsonDecode(clientConn, &resp); err != nil {
		t.Fatal(err)
	}
	<-done
	if resp.OK || len(ran) != 0 {
		t.Fatalf("a second backfill ran while the slot was taken: %+v", resp)
	}
	if len(mediaSlot) != 1 || len(sendSlot) != 1 {
		t.Fatal("a backfill must not touch the media or send slot")
	}
}

func jsonEncode(c net.Conn, v any) error { return json.NewEncoder(c).Encode(v) }
func jsonDecode(c net.Conn, v any) error { return json.NewDecoder(c).Decode(v) }
