package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/app"
)

const (
	channelsListKind  = "channels_list"
	channelsInfoKind  = "channels_info"
	channelsJoinKind  = "channels_join"
	channelsLeaveKind = "channels_leave"
	doctorKind        = "doctor"
)

type channelsListResult struct {
	Channels []channelRecord `json:"channels"`
}

type channelRecordResult struct {
	Channel channelRecord `json:"channel"`
}

type channelLeaveResult struct {
	JID string `json:"jid"`
}

// doctorLive is the part of `doctor` only the daemon knows: whether its
// connection is up right now.
type doctorLive struct {
	Connected bool   `json:"connected"`
	Authed    bool   `json:"authenticated"`
	LinkedJID string `json:"linked_jid,omitempty"`
}

func init() {
	registerKind(channelsListKind, kindHandler{readOnly: true, validate: func(sendDelegateRequest) error { return nil },
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			list, err := a.WA().GetSubscribedNewsletters(ctx)
			if err != nil {
				return nil, err
			}
			rows := channelRecords(list)
			if err := persistChannelRecords(a.DB(), rows); err != nil {
				return nil, err
			}
			return channelsListResult{Channels: rows}, nil
		}})
	registerKind(channelsInfoKind, kindHandler{readOnly: true, validate: validateChannelJID,
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			jid, _ := parseChannelJID(r.Chat)
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			meta, err := a.WA().GetNewsletterInfo(ctx, jid)
			if err != nil {
				return nil, err
			}
			if meta == nil {
				return nil, fmt.Errorf("channel not found")
			}
			row := channelRecordFromMeta(meta)
			if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
				return nil, err
			}
			return channelRecordResult{Channel: row}, nil
		}})
	registerKind(channelsJoinKind, kindHandler{validate: func(r sendDelegateRequest) error {
		code := strings.TrimSpace(r.Code)
		if code == "" {
			return fmt.Errorf("--invite is required")
		}
		if len(code) > 512 || strings.ContainsAny(code, " \t\r\n") {
			return fmt.Errorf("invalid --invite")
		}
		return nil
	}, run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
		if err := ensureLive(ctx, a); err != nil {
			return nil, err
		}
		meta, err := a.WA().GetNewsletterInfoWithInvite(ctx, strings.TrimSpace(r.Code))
		if err != nil {
			return nil, err
		}
		if meta == nil {
			return nil, fmt.Errorf("could not resolve channel from invite")
		}
		if err := a.WA().FollowNewsletter(ctx, meta.ID); err != nil {
			return nil, err
		}
		row := channelRecordFromMeta(meta)
		if err := persistChannelRecords(a.DB(), []channelRecord{row}); err != nil {
			return nil, err
		}
		return channelRecordResult{Channel: row}, nil
	}})
	registerKind(channelsLeaveKind, kindHandler{validate: validateChannelJID,
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			jid, _ := parseChannelJID(r.Chat)
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			if err := a.WA().UnfollowNewsletter(ctx, jid); err != nil {
				return nil, err
			}
			return channelLeaveResult{JID: jid.String()}, nil
		}})
	// Read-only: reports the daemon's live connection state, writes nothing.
	registerKind(doctorKind, kindHandler{readOnly: true, validate: func(sendDelegateRequest) error { return nil },
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			w := a.WA()
			live := doctorLive{Connected: w.IsConnected(), Authed: w.IsAuthed()}
			if live.Authed {
				live.LinkedJID = w.LinkedJID()
			}
			return live, nil
		}})
}

func validateChannelJID(r sendDelegateRequest) error {
	if err := requireText("--jid", r.Chat); err != nil {
		return err
	}
	_, err := parseChannelJID(r.Chat)
	return err
}
