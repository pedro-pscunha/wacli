package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openclaw/wacli/internal/store"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type BackfillOptions struct {
	ChatJID        string
	Count          int
	Requests       int
	WaitPerRequest time.Duration
	IdleExit       time.Duration
}

// lateOnDemandGrace bounds how long a finished backfill keeps listening for a
// reply that arrives after its wait ended.
const lateOnDemandGrace = 60 * time.Second

const (
	DefaultBackfillCount    = 50
	DefaultBackfillRequests = 1
	MaxBackfillCount        = 500
	MaxBackfillRequests     = 100
)

type BackfillResult struct {
	ChatJID        string
	RequestsSent   int
	ResponsesSeen  int
	MessagesAdded  int64
	MessagesSynced int64
}

type onDemandResponse struct {
	conversations int
	messages      int
	endType       waHistorySync.Conversation_EndOfHistoryTransferType
}

func (a *App) prepareBackfill(ctx context.Context, opts BackfillOptions) (types.JID, BackfillOptions, error) {
	chatStr := strings.TrimSpace(opts.ChatJID)
	if chatStr == "" {
		return types.JID{}, opts, fmt.Errorf("--chat is required")
	}
	chat, err := types.ParseJID(chatStr)
	if err != nil {
		return types.JID{}, opts, fmt.Errorf("parse chat JID: %w", err)
	}
	opts = normalizeBackfillOptions(opts)
	if err := validateBackfillOptions(opts); err != nil {
		return types.JID{}, opts, err
	}
	if err := a.EnsureAuthed(ctx); err != nil {
		return types.JID{}, opts, err
	}
	if err := a.OpenWA(); err != nil {
		return types.JID{}, opts, err
	}
	return chat, opts, nil
}

// backfillRunner holds the on-demand request loop. BackfillHistory drives it
// from a one-shot sync, BackfillHistoryLive from an already connected daemon.
type backfillRunner struct {
	handleOnDemand func(*events.HistorySync)
	run            func(ctx context.Context) error
	cleanup        func()
	requestsSent   *int
	responsesSeen  *int
	stored         *atomic.Int64
}

func (a *App) newBackfillRunner(ctx context.Context, opts BackfillOptions, chat types.JID) *backfillRunner {
	var mu sync.Mutex
	var waitCh chan onDemandResponse
	var manualMessagesStored atomic.Int64
	var manualLastEvent atomic.Int64
	manualLastEvent.Store(nowUTC().UnixNano())
	handleOnDemand := func(hs *events.HistorySync) {
		if hs == nil || hs.Data == nil || hs.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			return
		}
		for _, conv := range hs.Data.GetConversations() {
			if a.canonicalStoreJIDString(ctx, strings.TrimSpace(conv.GetID())) != a.canonicalStoreJID(ctx, chat).String() {
				continue
			}
			mu.Lock()
			ch := waitCh
			mu.Unlock()
			if ch == nil {
				return
			}
			resp := onDemandResponse{
				conversations: len(hs.Data.GetConversations()),
				messages:      len(conv.GetMessages()),
				endType:       conv.GetEndOfHistoryTransferType(),
			}
			select {
			case ch <- resp:
			default:
			}
			return
		}
	}
	handlerID := a.wa.AddEventHandler(func(evt any) {
		switch v := evt.(type) {
		case *events.Message:
			notif := historySyncNotificationFromMessage(v)
			if notif == nil || notif.GetSyncType() != waE2E.HistorySyncType_ON_DEMAND {
				return
			}
			// A late reply still downloads after the request's context ended.
			dlCtx, dlCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer dlCancel()
			data, err := a.wa.DownloadHistorySync(dlCtx, notif)
			if err != nil {
				a.emitWarning(
					"on_demand_history_download_failed",
					fmt.Sprintf("warning: failed to download on-demand history sync: %v", err),
					map[string]any{"error": err.Error()},
				)
				return
			}
			if data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
				return
			}
			hs := &events.HistorySync{Data: data}
			// Store as the running sync would: its options, storage limits and
			// media queue. A reply that comes after the request ended is still
			// stored, so the work is not lost; only the waiting stops.
			syncOpts, limits, enqueue := SyncOptions{}, (*syncStorageLimits)(nil), func(string, string) {}
			if live := a.liveSync.Load(); live != nil {
				syncOpts, limits, enqueue = live.opts, live.limits, live.enqueueMedia
			}
			storeCtx := context.WithoutCancel(ctx)
			if limits != nil {
				a.handleHistorySync(storeCtx, syncOpts, hs, &manualMessagesStored, &manualLastEvent, enqueue, limits)
			} else {
				a.handleHistorySync(storeCtx, syncOpts, hs, &manualMessagesStored, &manualLastEvent, enqueue)
			}
			if err := a.wa.DeleteHistorySyncMedia(storeCtx, notif); err != nil {
				a.emitWarning(
					"history_delete_failed",
					fmt.Sprintf("warning: failed to delete history sync media: %v", err),
					map[string]any{"error": err.Error()},
				)
			}
			handleOnDemand(hs)
		}
	})
	// A reply can arrive after the last wait gave up. Keep the handler for one
	// more wait period so that late blob is stored instead of dropped; the sync
	// handler ignores ON_DEMAND blobs, so nothing else would store it.
	cleanup := func() {
		tail := opts.WaitPerRequest
		if tail <= 0 {
			tail = 0
		}
		if tail > lateOnDemandGrace {
			tail = lateOnDemandGrace
		}
		if tail == 0 {
			a.wa.RemoveEventHandler(handlerID)
			return
		}
		time.AfterFunc(tail, func() { a.wa.RemoveEventHandler(handlerID) })
	}

	requestsSent := new(int)
	responsesSeen := new(int)
	errResponseTimeout := errors.New("timed out waiting for on-demand history sync response")
	request := func(ctx context.Context, anchor store.MessageInfo, requestChat types.JID) (onDemandResponse, error) {
		if err := ctx.Err(); err != nil {
			return onDemandResponse{}, err
		}
		ch := make(chan onDemandResponse, 4)
		mu.Lock()
		waitCh = ch
		mu.Unlock()
		defer func() {
			mu.Lock()
			waitCh = nil
			mu.Unlock()
		}()

		storeChat := a.canonicalStoreJID(ctx, chat).String()
		*requestsSent++
		a.emitOrPrint("backfill_requesting", map[string]any{
			"chat_jid":         storeChat,
			"request_chat_jid": requestChat.String(),
			"count":            opts.Count,
			"request":          *requestsSent,
			"anchor_msg_id":    anchor.MsgID,
		}, "Requesting %d older messages for %s...\n", opts.Count, storeChat)
		reqInfo := types.MessageInfo{
			MessageSource: types.MessageSource{Chat: requestChat, IsFromMe: anchor.FromMe},
			ID:            types.MessageID(anchor.MsgID),
			Timestamp:     anchor.Timestamp,
		}
		if _, err := a.wa.RequestHistorySyncOnDemand(ctx, reqInfo, opts.Count); err != nil {
			return onDemandResponse{}, err
		}
		timer := time.NewTimer(opts.WaitPerRequest)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return onDemandResponse{}, ctx.Err()
		case resp := <-ch:
			*responsesSeen++
			return resp, nil
		case <-timer.C:
			return onDemandResponse{}, fmt.Errorf("%w (anchor %s)", errResponseTimeout, anchor.MsgID)
		}
	}

	// A mapped 1:1 chat has two identities. The primary device files some
	// chats under the LID and others under the phone number, and answers only
	// requests addressed to the one it uses (#444). Ask by LID first, retry the
	// same anchor by phone number when that goes unanswered, and keep using
	// the identity of the successful attempt. Late replies have no request ID,
	// so either identity remains eligible for fallback in later batches. Resolve on every
	// request: sync can learn a mapping while connecting.
	var preferPN bool
	requestIdentities := func(ctx context.Context) []types.JID {
		lidChat := a.wa.ResolvePNToLID(ctx, chat)
		pnChat := a.wa.ResolveLIDToPN(ctx, lidChat)
		if pnChat == lidChat {
			return []types.JID{lidChat}
		}
		if preferPN {
			return []types.JID{pnChat, lidChat}
		}
		return []types.JID{lidChat, pnChat}
	}
	requestAnchor := func(ctx context.Context, anchor store.MessageInfo) (onDemandResponse, error) {
		ids := requestIdentities(ctx)
		resp, err := request(ctx, anchor, ids[0])
		if len(ids) < 2 || !errors.Is(err, errResponseTimeout) || ctx.Err() != nil {
			return resp, err
		}
		a.emitWarning("backfill_identity_retry",
			fmt.Sprintf("warning: no history response for anchor %s from %s; retrying with %s", anchor.MsgID, ids[0], ids[1]),
			map[string]any{
				"chat_jid":               a.canonicalStoreJID(ctx, chat).String(),
				"anchor_msg_id":          anchor.MsgID,
				"request_chat_jid":       ids[0].String(),
				"retry_request_chat_jid": ids[1].String(),
			})
		resp, err = request(ctx, anchor, ids[1])
		if err == nil {
			preferPN = ids[1].Server == types.DefaultUserServer
		}
		return resp, err
	}

	return &backfillRunner{
		handleOnDemand: handleOnDemand,
		cleanup:        cleanup,
		requestsSent:   requestsSent,
		responsesSeen:  responsesSeen,
		stored:         &manualMessagesStored,
		run: func(ctx context.Context) error {
			// Sync can learn mappings and migrate old LID rows while connecting.
			// Resolve the local identity only after that migration has completed.
			chatStr := a.canonicalStoreJID(ctx, chat).String()
			for i := 0; i < opts.Requests; i++ {
				oldest, err := a.db.GetOldestMessageInfo(chatStr)
				if err != nil {
					if err == sql.ErrNoRows {
						return fmt.Errorf("no messages for %s in local DB; run `wacli sync` first", chatStr)
					}
					return err
				}

				resp, err := requestAnchor(ctx, oldest)
				if errors.Is(err, errResponseTimeout) && ctx.Err() == nil {
					next, nextErr := a.db.GetNextMessageInfo(chatStr, oldest.MsgID)
					if nextErr != nil && !errors.Is(nextErr, sql.ErrNoRows) {
						return nextErr
					}
					if nextErr == nil {
						a.emitWarning("backfill_anchor_retry",
							fmt.Sprintf("warning: no history response for anchor %s; retrying once with next local anchor %s", oldest.MsgID, next.MsgID),
							map[string]any{"chat_jid": chatStr, "anchor_msg_id": oldest.MsgID, "retry_anchor_msg_id": next.MsgID})
						resp, err = requestAnchor(ctx, next)
					}
				}
				if err != nil {
					return err
				}

				a.emitOrPrint("backfill_response", map[string]any{
					"chat_jid":       chatStr,
					"conversations":  resp.conversations,
					"messages":       resp.messages,
					"responses_seen": *responsesSeen,
				}, "On-demand history sync: %d conversations, %d messages.\n", resp.conversations, resp.messages)

				newOldest, err := a.db.GetOldestMessageInfo(chatStr)
				// A retry's newer anchor is not progress past the original oldest row.
				if err == nil && newOldest.MsgID == oldest.MsgID {
					a.emitOrPrint("backfill_stopped", map[string]any{
						"chat_jid": chatStr,
						"reason":   "no_older_messages_added",
					}, "No older messages were added (stopping).\n")
					return nil
				}
				if resp.messages <= 0 {
					a.emitOrPrint("backfill_stopped", map[string]any{
						"chat_jid": chatStr,
						"reason":   "no_messages_returned",
					}, "No messages returned (stopping).\n")
					return nil
				}
				if resp.endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
					a.emitOrPrint("backfill_stopped", map[string]any{
						"chat_jid": chatStr,
						"reason":   "start_of_history_reached",
					}, "Reached start of chat history (stopping).\n")
					return nil
				}
			}
			return nil
		},
	}
}

func (a *App) BackfillHistory(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	chat, opts, err := a.prepareBackfill(ctx, opts)
	if err != nil {
		return BackfillResult{}, err
	}
	a.wa.SetManualHistorySyncDownload(true)
	defer a.wa.SetManualHistorySyncDownload(false)

	beforeCount, _ := a.db.CountMessages()
	r := a.newBackfillRunner(ctx, opts, chat)
	defer r.cleanup()

	syncRes, err := a.Sync(ctx, SyncOptions{
		Mode:             SyncModeOnce,
		AllowQR:          false,
		IdleExit:         opts.IdleExit,
		afterHistorySync: r.handleOnDemand,
		AfterConnect:     r.run,
	})
	if err != nil {
		return BackfillResult{}, err
	}

	afterCount, _ := a.db.CountMessages()

	return BackfillResult{
		ChatJID:        a.canonicalStoreJID(ctx, chat).String(),
		RequestsSent:   *r.requestsSent,
		ResponsesSeen:  *r.responsesSeen,
		MessagesAdded:  afterCount - beforeCount,
		MessagesSynced: syncRes.MessagesStored,
	}, nil
}

// BackfillHistoryLive requests older messages on the connection the caller
// already holds, which is how the sync daemon repairs a gap without a second
// session. Messages are stored through the normal history-sync path. The
// caller must be the connected sync process, which already downloads history
// payloads manually.
func (a *App) BackfillHistoryLive(ctx context.Context, opts BackfillOptions) (BackfillResult, error) {
	chat, opts, err := a.prepareBackfill(ctx, opts)
	if err != nil {
		return BackfillResult{}, err
	}
	if !a.wa.IsConnected() {
		return BackfillResult{}, errors.New("not connected")
	}
	r := a.newBackfillRunner(ctx, opts, chat)
	defer r.cleanup()
	if err := r.run(ctx); err != nil {
		return BackfillResult{}, err
	}
	// Count only what this backfill stored. A whole-database difference would
	// include live traffic stored during the run and could report a repair that
	// did not happen.
	stored := r.stored.Load()
	return BackfillResult{
		ChatJID:        a.canonicalStoreJID(ctx, chat).String(),
		RequestsSent:   *r.requestsSent,
		ResponsesSeen:  *r.responsesSeen,
		MessagesAdded:  stored,
		MessagesSynced: stored,
	}, nil
}

// ValidateBackfillRequest checks a backfill request the way BackfillHistory
// does, without touching the store or the network.
func ValidateBackfillRequest(opts BackfillOptions) error {
	if strings.TrimSpace(opts.ChatJID) == "" {
		return fmt.Errorf("--chat is required")
	}
	if _, err := types.ParseJID(strings.TrimSpace(opts.ChatJID)); err != nil {
		return fmt.Errorf("parse chat JID: %w", err)
	}
	return validateBackfillOptions(normalizeBackfillOptions(opts))
}

func normalizeBackfillOptions(opts BackfillOptions) BackfillOptions {
	if opts.Count <= 0 {
		opts.Count = DefaultBackfillCount
	}
	if opts.Requests <= 0 {
		opts.Requests = DefaultBackfillRequests
	}
	if opts.WaitPerRequest <= 0 {
		opts.WaitPerRequest = 60 * time.Second
	}
	if opts.IdleExit <= 0 {
		opts.IdleExit = 5 * time.Second
	}
	return opts
}

func validateBackfillOptions(opts BackfillOptions) error {
	if opts.Count > MaxBackfillCount {
		return fmt.Errorf("--count must be <= %d (got %d)", MaxBackfillCount, opts.Count)
	}
	if opts.Requests > MaxBackfillRequests {
		return fmt.Errorf("--requests must be <= %d (got %d)", MaxBackfillRequests, opts.Requests)
	}
	return nil
}
