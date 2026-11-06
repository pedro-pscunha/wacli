package main

import (
	"strings"
	"testing"

	"github.com/openclaw/wacli/internal/lock"
)

// runKindViaSocket runs args against a fake daemon that answers with data and
// returns the request the daemon saw, plus stdout.
func runKindViaSocket(t *testing.T, data string, produced string, args ...string) (sendDelegateRequest, string) {
	t.Helper()
	skipPresenceDelegateSocketTestOnUnsupportedOS(t)
	storeDir := shortPresenceDelegateStoreDir(t)
	lk, err := lock.Acquire(storeDir)
	if err != nil {
		t.Fatalf("lock store: %v", err)
	}
	defer lk.Release()
	server := startPresenceDelegateTestSocket(t, storeDir, func(req sendDelegateRequest) sendDelegateResponse {
		return sendDelegateResponse{OK: true, Sent: produced != "", ID: produced, Data: []byte(data)}
	})
	defer server.stop()
	full := append([]string{"--store", storeDir, "--json", "--timeout", "750ms"}, args...)
	stdout, stderr, err := runPresenceDelegateHelper(t, full)
	if err != nil {
		t.Fatalf("%v failed: %v stdout=%q stderr=%q", args, err, stdout, stderr)
	}
	if strings.Contains(stderr, "store is locked") {
		t.Fatalf("%v tried the direct store path: %q", args, stderr)
	}
	return server.nextRequest(t), stdout
}

// delegateOnlyRefusals lists one invocation per command that connects. Each
// must be refused with --delegate-only when no daemon runs.
var delegateOnlyRefusals = [][]string{}

func registerRefusal(args ...string) { delegateOnlyRefusals = append(delegateOnlyRefusals, args) }

func TestEveryNewKindCommandIsRefusedWithoutDaemonWhenDelegateOnly(t *testing.T) {
	for _, c := range delegateOnlyRefusals {
		store := t.TempDir()
		err := execute(append([]string{"--store", store, "--delegate-only"}, c...))
		if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
			t.Errorf("%v: err = %v, want delegate-only refusal", c, err)
		}
	}
}

func TestRegisteredKindsAppearInCapabilities(t *testing.T) {
	have := map[string]bool{}
	for _, k := range currentCapabilities().Kinds {
		have[k] = true
	}
	for kind := range delegateHandlers {
		if !have[kind] {
			t.Errorf("kind %q missing from capabilities", kind)
		}
	}
}

func TestRegisteredKindsValidateBeforeAnyIO(t *testing.T) {
	for kind, h := range delegateHandlers {
		if h.validate == nil {
			t.Errorf("kind %q has no validator", kind)
			continue
		}
		if err := h.validate(sendDelegateRequest{Kind: kind}); err == nil && !validatesEmpty[kind] {
			t.Errorf("kind %q accepts an empty request", kind)
		}
	}
}

// validatesEmpty names the kinds whose empty request is valid.
var validatesEmpty = map[string]bool{}
