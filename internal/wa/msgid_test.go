package wa

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestValidateMessageID(t *testing.T) {
	for _, ok := range []string{"3EB0ABCDEF0123456789", "abcdefgh", "A1B2C3D4E5"} {
		if err := ValidateMessageID(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "short", "has space 12345", "semi;colon123", "ÿÿÿÿÿÿÿÿÿ"} {
		if err := ValidateMessageID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSendExtras(t *testing.T) {
	if got := sendExtras(context.Background(), ""); got != nil {
		t.Fatalf("default must add no extra, got %v", got)
	}
	ctx := WithMessageID(context.Background(), "3EB0ABCDEF0123456789")
	got := sendExtras(ctx, "")
	if len(got) != 1 || got[0].ID != "3EB0ABCDEF0123456789" {
		t.Fatalf("extras = %+v", got)
	}
	got = sendExtras(ctx, "handle")
	if len(got) != 1 || got[0].ID == "" || got[0].MediaHandle != "handle" {
		t.Fatalf("id and media handle must share one extra, got %+v", got)
	}
	if WithMessageID(context.Background(), "") != context.Background() {
		t.Fatal("empty id must leave the context unchanged")
	}
}

func TestMergeOwnDevices(t *testing.T) {
	self := types.JID{User: "5511999999999", Device: 7, Server: types.DefaultUserServer}
	others := []types.JID{
		{User: "5511999999999", Device: 0, Server: types.DefaultUserServer},
		{User: "5511999999999", Device: 3, Server: types.DefaultUserServer},
		{User: "5511999999999", Device: 3, Server: types.DefaultUserServer},
	}
	got := mergeOwnDevices(self, "linux", "air", others)
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Device != 0 || got[1].Device != 3 || got[2].Device != 7 {
		t.Fatalf("not sorted: %+v", got)
	}
	if !got[2].Self || got[2].Platform != "linux" || got[2].Name != "air" || got[0].Self {
		t.Fatalf("self data wrong: %+v", got)
	}
}
