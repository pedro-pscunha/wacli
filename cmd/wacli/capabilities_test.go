package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/wa"
)

// TestDelegateKindsAreAllAccepted keeps delegateKinds in step with the switch
// in executeDelegatedSend: every listed kind must not hit the default branch,
// and every case label of the switch must be listed.
func TestDelegateKindsAreAllAccepted(t *testing.T) {
	for _, kind := range allDelegateKinds() {
		_, err := executeDelegatedSend(context.Background(), nil, sendDelegateRequest{Version: 99, Kind: kind})
		if err == nil || !strings.Contains(err.Error(), "unsupported send delegate version") {
			t.Fatalf("version gate must run first for %q: %v", kind, err)
		}
	}
	src, err := os.ReadFile("send_ipc.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "switch req.Kind {")
	end := strings.Index(body[start:], "default:")
	block := body[start : start+end]
	listed := map[string]bool{}
	for _, k := range allDelegateKinds() {
		listed[k] = true
	}
	literal := regexp.MustCompile(`case ([^:\n]+):`)
	for _, m := range literal.FindAllStringSubmatch(block, -1) {
		for _, label := range strings.Split(m[1], ",") {
			label = strings.TrimSpace(label)
			if strings.HasPrefix(label, `"`) {
				if !listed[strings.Trim(label, `"`)] {
					t.Errorf("kind %s is accepted but missing from delegateKinds", label)
				}
			}
		}
	}
}

func TestCapabilitiesContent(t *testing.T) {
	c := currentCapabilities()
	if c.Protocol != sendDelegateVersion || c.Version == "" {
		t.Fatalf("capabilities = %+v", c)
	}
	have := map[string]bool{}
	for _, k := range c.Kinds {
		have[k] = true
	}
	for _, want := range []string{"text", "file", capabilitiesKind, devicesKind, mediaDownloadKind, labelChatKind} {
		if !have[want] {
			t.Errorf("kinds missing %q", want)
		}
	}
}

func TestCapabilitiesAndDevicesDelegateThroughSocket(t *testing.T) {
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Release()

	server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Capabilities: currentCapabilities()}
	})
	stdout, stderr, err := runPresenceDelegateHelper(t, []string{"--store", storeDir, "--json", "--timeout", "750ms", "capabilities"})
	server.stop()
	if err != nil {
		t.Fatalf("capabilities: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if req := server.nextRequest(t); req.Kind != capabilitiesKind {
		t.Fatalf("kind = %q", req.Kind)
	}
	var env struct {
		Data delegateCapabilities `json:"data"`
	}
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&env); err != nil || env.Data.Protocol != sendDelegateVersion || len(env.Data.Kinds) == 0 {
		t.Fatalf("stdout = %q (%v)", stdout, err)
	}

	server = startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Devices: []wa.OwnDevice{{JID: "5511@s.whatsapp.net", Device: 0}, {JID: "5511:7@s.whatsapp.net", Device: 7, Self: true, Platform: "linux", Name: "air"}}}
	})
	defer server.stop()
	stdout, stderr, err = runPresenceDelegateHelper(t, []string{"--store", storeDir, "--json", "--timeout", "750ms", "devices"})
	if err != nil {
		t.Fatalf("devices: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if req := server.nextRequest(t); req.Kind != devicesKind {
		t.Fatalf("kind = %q", req.Kind)
	}
	if !strings.Contains(stdout, `"count":2`) || !strings.Contains(stdout, `"platform":"linux"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCapabilitiesWithoutDaemonFailsAndNeverConnects(t *testing.T) {
	store := t.TempDir()
	err := execute([]string{"--store", store, "capabilities"})
	if err == nil || !strings.Contains(err.Error(), "daemon not running") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevicesRefusedWithoutDaemonWhenDelegateOnly(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "--delegate-only", "devices"})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadOnlyKindsSkipTheSendQueue(t *testing.T) {
	if !isReadOnlyDelegateKind(capabilitiesKind) || !isReadOnlyDelegateKind(devicesKind) || isReadOnlyDelegateKind("text") {
		t.Fatal("read-only kind set is wrong")
	}
}
