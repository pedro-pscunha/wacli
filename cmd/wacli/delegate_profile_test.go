package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() {
	registerRefusal("profile", "set-name", "Pedro")
	registerRefusal("profile", "set-about", "hello")
	registerRefusal("profile", "remove-picture")
	registerRefusal("profile", "get-about", "--jid", "5511999999999")
	registerRefusal("profile", "picture-info", "--jid", "5511999999999")
	registerRefusal("profile", "business", "--jid", "5511999999999")
}

func writeTestJPEG(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "p.jpg")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProfileSetPictureRefusedWithoutDaemonWhenDelegateOnly(t *testing.T) {
	err := execute([]string{"--store", t.TempDir(), "--delegate-only", "profile", "set-picture", writeTestJPEG(t)})
	if err == nil || !strings.Contains(err.Error(), "refusing to connect outside the sync daemon") {
		t.Fatalf("err = %v", err)
	}
}

func TestProfileKindValidators(t *testing.T) {
	v := func(kind string, r sendDelegateRequest) error { return delegateHandlers[kind].validate(r) }
	if v(profileSetNameKind, sendDelegateRequest{Name: "Pedro"}) != nil || v(profileSetNameKind, sendDelegateRequest{Name: "  "}) == nil {
		t.Error("set-name blank handling")
	}
	if v(profileSetNameKind, sendDelegateRequest{Name: strings.Repeat("x", 26)}) == nil {
		t.Error("set-name accepts 26 characters")
	}
	if v(profileSetAboutKind, sendDelegateRequest{About: strings.Repeat("x", 140)}) == nil || v(profileSetAboutKind, sendDelegateRequest{About: strings.Repeat("é", 139)}) != nil {
		t.Error("about limit counts characters, not bytes")
	}
	if v(profileSetPictureKind, sendDelegateRequest{}) == nil || v(profileSetPictureKind, sendDelegateRequest{Picture: []byte("PNG not jpeg")}) == nil {
		t.Error("picture must be a JPEG")
	}
	if v(profileSetPictureKind, sendDelegateRequest{Picture: []byte{0xFF, 0xD8, 0xFF, 0xE0}}) != nil {
		t.Error("a JPEG header is valid")
	}
	if v(profileSetPictureKind, sendDelegateRequest{Picture: append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, profileMaxPictureBytes)...)}) == nil {
		t.Error("an oversized picture is accepted")
	}
	if v(profileGetAboutKind, sendDelegateRequest{}) == nil || v(profileBusinessKind, sendDelegateRequest{To: "5511999999999"}) != nil {
		t.Error("target handling")
	}
	if v(profilePictureInfoKind, sendDelegateRequest{To: "5511999999999", ExistingID: "a b"}) == nil {
		t.Error("existing id with a space")
	}
}

func init() { validatesEmpty[profileRemovePictureKind] = true }

func TestProfileKindsDelegateThroughSocket(t *testing.T) {
	req, _ := runKindViaSocket(t, `{"name":"Pedro"}`, "", "profile", "set-name", "Pedro")
	if req.Kind != profileSetNameKind || req.Name != "Pedro" {
		t.Fatalf("req = %+v", req)
	}
	req, stdout := runKindViaSocket(t, `{"jid":"5511999999999@s.whatsapp.net","about":"hi"}`, "", "profile", "get-about", "--jid", "5511999999999")
	if req.Kind != profileGetAboutKind || req.To != "5511999999999" || !strings.Contains(stdout, `"about":"hi"`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
	req, _ = runKindViaSocket(t, `{"picture_id":"P1"}`, "", "profile", "set-picture", writeTestJPEG(t))
	if req.Kind != profileSetPictureKind || len(req.Picture) < 3 || req.Picture[0] != 0xFF {
		t.Fatalf("req kind=%q picture bytes=%d", req.Kind, len(req.Picture))
	}
	req, stdout = runKindViaSocket(t, `{"jid":"5511999999999@s.whatsapp.net","unchanged":true}`, "", "profile", "picture-info", "--jid", "5511999999999", "--existing-id", "OLD1", "--preview")
	if req.Kind != profilePictureInfoKind || req.ExistingID != "OLD1" || !req.Preview || !strings.Contains(stdout, `"unchanged":true`) {
		t.Fatalf("req = %+v stdout=%q", req, stdout)
	}
}
