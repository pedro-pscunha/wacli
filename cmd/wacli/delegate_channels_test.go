package main

import (
	"strings"
	"testing"
)

const testChannel = "120363000000000002@newsletter"

func init() {
	registerRefusal("channels", "list")
	registerRefusal("channels", "info", "--jid", testChannel)
	registerRefusal("channels", "join", "--invite", "https://whatsapp.com/channel/ABCDEF")
	registerRefusal("channels", "leave", "--jid", testChannel)
	validatesEmpty[channelsListKind] = true
	validatesEmpty[doctorKind] = true
}

func TestChannelKindValidators(t *testing.T) {
	v := func(kind string, r sendDelegateRequest) error { return delegateHandlers[kind].validate(r) }
	if v(channelsInfoKind, sendDelegateRequest{Chat: testChannel}) != nil || v(channelsLeaveKind, sendDelegateRequest{Chat: testChannel}) != nil {
		t.Error("a channel JID is valid")
	}
	for _, bad := range []string{"", "123@g.us", "123@s.whatsapp.net"} {
		if v(channelsInfoKind, sendDelegateRequest{Chat: bad}) == nil || v(channelsLeaveKind, sendDelegateRequest{Chat: bad}) == nil {
			t.Errorf("accepted %q as a channel", bad)
		}
	}
	if v(channelsJoinKind, sendDelegateRequest{Code: "ABC123"}) != nil || v(channelsJoinKind, sendDelegateRequest{}) == nil || v(channelsJoinKind, sendDelegateRequest{Code: "a b"}) == nil {
		t.Error("join invite handling")
	}
}

func TestChannelKindsDelegateThroughSocket(t *testing.T) {
	row := `{"jid":"` + testChannel + `","name":"News","state":"active","subscribers":5}`
	req, stdout := runKindViaSocket(t, `{"channels":[`+row+`]}`, "", "channels", "list")
	if req.Kind != channelsListKind || !strings.Contains(stdout, `"name":"News"`) {
		t.Fatalf("req=%+v stdout=%q", req, stdout)
	}
	req, _ = runKindViaSocket(t, `{"channel":`+row+`}`, "", "channels", "info", "--jid", testChannel)
	if req.Kind != channelsInfoKind || req.Chat != testChannel {
		t.Fatalf("req=%+v", req)
	}
	req, stdout = runKindViaSocket(t, `{"channel":`+row+`}`, "", "channels", "join", "--invite", "ABC123")
	if req.Kind != channelsJoinKind || req.Code != "ABC123" || !strings.Contains(stdout, `"joined":true`) {
		t.Fatalf("req=%+v stdout=%q", req, stdout)
	}
	req, stdout = runKindViaSocket(t, `{"jid":"`+testChannel+`"}`, "", "channels", "leave", "--jid", testChannel)
	if req.Kind != channelsLeaveKind || !strings.Contains(stdout, `"left":true`) {
		t.Fatalf("req=%+v stdout=%q", req, stdout)
	}
}

func TestDoctorKindIsReadOnlyAndRegistered(t *testing.T) {
	h, ok := delegateHandlers[doctorKind]
	if !ok || !h.readOnly || h.bulk {
		t.Fatalf("doctor handler = %+v ok=%v", h, ok)
	}
	if !isReadOnlyDelegateKind(doctorKind) {
		t.Fatal("doctor must skip the send queue")
	}
}

func TestDoctorReportsDaemonConnectionWhenLockHeld(t *testing.T) {
	_, stdout := runKindViaSocket(t, `{"connected":true,"authenticated":true,"linked_jid":"5511@s.whatsapp.net"}`, "", "doctor")
	for _, want := range []string{`"connected":true`, `"authenticated":true`, `"connection_state":"connected"`, `"linked_jid":"5511@s.whatsapp.net"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor output lacks %s: %q", want, stdout)
		}
	}
}
