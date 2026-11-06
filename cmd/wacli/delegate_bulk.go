package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

const (
	historyBackfillKind = "history_backfill"
	mediaBackfillKind   = "media_backfill"
	labelsListKind      = "labels_list"
)

// maxBackfillMessages caps one history backfill request per chat: count times
// requests. The app limits each factor on its own (500 and 100).
const maxBackfillMessages = 5000

type historyBackfillResult struct {
	Chat           string `json:"chat"`
	RequestsSent   int    `json:"requests_sent"`
	ResponsesSeen  int    `json:"responses_seen"`
	MessagesAdded  int64  `json:"messages_added"`
	MessagesSynced int64  `json:"messages_synced"`
}

type mediaBackfillResult struct {
	Pending    int `json:"pending"`
	Attempted  int `json:"attempted"`
	Downloaded int `json:"downloaded"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

type labelsListResult struct {
	Labels []labelInfo `json:"labels"`
}

func init() {
	registerKind(historyBackfillKind, kindHandler{slot: slotBulk, validate: validateHistoryBackfill, run: runHistoryBackfill})
	registerKind(mediaBackfillKind, kindHandler{slot: slotBulk, validate: validateMediaBackfill, run: runMediaBackfill})
	registerKind(labelsListKind, kindHandler{skipsQueue: true, validate: func(sendDelegateRequest) error { return nil }, run: runLabelsListKind})
}

func historyOptions(r sendDelegateRequest) app.BackfillOptions {
	return app.BackfillOptions{
		ChatJID:        strings.TrimSpace(r.Chat),
		Count:          r.Count,
		Requests:       r.Requests,
		WaitPerRequest: millisDuration(r.WaitMS, 0),
		IdleExit:       millisDuration(r.IdleExitMS, 0),
	}
}

func validateHistoryBackfill(r sendDelegateRequest) error {
	if r.Count < 0 || r.Requests < 0 || r.WaitMS < 0 || r.IdleExitMS < 0 {
		return fmt.Errorf("--count, --requests, --wait and --idle-exit must not be negative")
	}
	opts := historyOptions(r)
	if err := app.ValidateBackfillRequest(opts); err != nil {
		return err
	}
	count, requests := opts.Count, opts.Requests
	if count == 0 {
		count = app.DefaultBackfillCount
	}
	if requests == 0 {
		requests = app.DefaultBackfillRequests
	}
	if count*requests > maxBackfillMessages {
		return fmt.Errorf("--count times --requests must be at most %d messages per chat (got %d)", maxBackfillMessages, count*requests)
	}
	return nil
}

func runHistoryBackfill(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	var (
		res app.BackfillResult
		err error
	)
	if ctx.Value(daemonConnKey{}) != nil {
		// The daemon holds the only connection: request on it, never reconnect.
		res, err = a.BackfillHistoryLive(ctx, historyOptions(r))
	} else {
		res, err = a.BackfillHistory(ctx, historyOptions(r))
	}
	if err != nil {
		return nil, err
	}
	return historyBackfillResult{
		Chat: res.ChatJID, RequestsSent: res.RequestsSent, ResponsesSeen: res.ResponsesSeen,
		MessagesAdded: res.MessagesAdded, MessagesSynced: res.MessagesSynced,
	}, nil
}

func validateMediaBackfill(r sendDelegateRequest) error {
	if r.Limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if r.Workers < 0 || r.Workers > 16 {
		return fmt.Errorf("--workers must be between 0 and 16")
	}
	if c := strings.TrimSpace(r.Chat); c != "" {
		jid, err := types.ParseJID(c)
		if err != nil {
			return fmt.Errorf("parse --chat: %w", err)
		}
		// ParseJID takes any string without an @ as a user on the default
		// server. A chat filter must name a real chat: user and known server.
		switch jid.Server {
		case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer, types.NewsletterServer:
		default:
			return fmt.Errorf("--chat %q is not a user, group or channel JID", c)
		}
		if jid.User == "" || strings.ContainsAny(jid.User, " \t%") {
			return fmt.Errorf("--chat %q has an invalid user part", c)
		}
	}
	return nil
}

func runMediaBackfill(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	res, err := a.BackfillMedia(ctx, app.BackfillMediaOptions{ChatJID: r.Chat, Limit: r.Limit, Workers: r.Workers})
	if err != nil {
		return nil, err
	}
	return mediaBackfillResult{Pending: res.Pending, Attempted: res.Attempted, Downloaded: res.Downloaded, Skipped: res.Skipped, Failed: res.Failed}, nil
}

func runLabelsListKind(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	mutations, err := a.ReadLabelMutations(ctx)
	if err != nil {
		return nil, err
	}
	rows := collectLabels(mutations)
	resolveLabelChatNames(ctx, a, rows)
	return labelsListResult{Labels: rows}, nil
}

// bulkTimeout returns the command timeout for a long backfill. An explicit
// --timeout wins. Otherwise the default 5-minute timeout would cut a run that
// legitimately takes an hour, so use the bound the request implies.
func bulkTimeout(cmd *cobra.Command, flags *rootFlags, implied time.Duration) *rootFlags {
	f := *flags
	if !mediaBulkTimeoutEnabled(cmd, flags) {
		f.timeout = implied
	}
	return &f
}
