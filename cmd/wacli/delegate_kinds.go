package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/app"
)

// A registered kind is a command that must run on the daemon's connection when
// a daemon runs, and on its own connection otherwise (never with
// --delegate-only). Each kind has one validator, run by the client before any
// I/O and again by the daemon, and one handler, run by whichever process holds
// the connection. The handler's result travels back as sendDelegateResponse.Data.
type kindHandler struct {
	validate func(sendDelegateRequest) error
	run      func(context.Context, *app.App, sendDelegateRequest) (any, error)
	// skipsQueue kinds neither queue behind sends nor take a paced slot. They
	// can still write the local database (group info, channel list): the name
	// says only that they do not wait on the send queue.
	skipsQueue bool
	// slot says which queue the kind runs on. The zero value is the send queue.
	slot slotKind
}

type slotKind int

const (
	slotSend slotKind = iota
	// slotBulk gives the kind a slot of its own: a long backfill.
	slotBulk
)

var delegateHandlers = map[string]kindHandler{}

func registerKind(kind string, h kindHandler) {
	if _, dup := delegateHandlers[kind]; dup {
		panic("duplicate delegate kind " + kind)
	}
	delegateHandlers[kind] = h
}

func isLongBulkKind(kind string) bool {
	h, ok := delegateHandlers[kind]
	return ok && h.slot == slotBulk
}

func isBulkKind(kind string) bool {
	return kind == mediaDownloadKind || isLongBulkKind(kind)
}

// producedID is implemented by results that put a message on the wire.
type producedID interface{ ProducedID() string }

type daemonConnKey struct{}

// ensureLive connects a command that runs on its own connection. In the daemon
// the connection already exists: it is never opened or reopened from a handler.
func ensureLive(ctx context.Context, a *app.App) error {
	if ctx.Value(daemonConnKey{}) != nil {
		if !a.WA().IsConnected() {
			return errors.New("the sync daemon is not connected to WhatsApp right now")
		}
		return nil
	}
	if a.WA().IsConnected() {
		return nil
	}
	return a.Connect(ctx, false, nil)
}

func validateDelegateRequest(req sendDelegateRequest) error {
	if h, ok := delegateHandlers[req.Kind]; ok && h.validate != nil {
		return h.validate(req)
	}
	return nil
}

func executeRegisteredKind(ctx context.Context, a *app.App, req sendDelegateRequest) (sendDelegateResponse, error) {
	h, ok := delegateHandlers[req.Kind]
	if !ok {
		return sendDelegateResponse{}, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	res, err := h.run(ctx, a, req)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return resultResponse(res)
}

func resultResponse(res any) (sendDelegateResponse, error) {
	raw, err := json.Marshal(res)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	resp := sendDelegateResponse{OK: true, Data: raw}
	if p, ok := res.(producedID); ok {
		resp.Sent = true
		resp.ID = p.ProducedID()
	}
	return resp, nil
}

// sendMeta carries the response-level facts about a send into a typed result:
// the daemon answers them beside `data`, and a result that dropped them would
// lose the retry and mismatch warnings the caller keys on.
type sendMeta struct {
	AlreadySent    bool   `json:"already_sent,omitempty"`
	TargetMismatch string `json:"target_mismatch,omitempty"`
	IDRequested    string `json:"id_requested,omitempty"`
}

func (m *sendMeta) setSendMeta(r sendDelegateResponse) {
	m.AlreadySent, m.TargetMismatch, m.IDRequested = r.AlreadySent, r.TargetMismatch, r.IDMismatch
}

type sendMetaSetter interface{ setSendMeta(sendDelegateResponse) }

func applySendMeta(out any, r sendDelegateResponse) {
	if m, ok := out.(sendMetaSetter); ok {
		m.setSendMeta(r)
	}
}

// callKind validates a registered kind, then runs it on the daemon (lock held
// by it) or on this process's own connection. The result type is the same on
// both paths.
func callKind[T any](ctx context.Context, flags *rootFlags, req sendDelegateRequest) (T, error) {
	var zero T
	h, ok := delegateHandlers[req.Kind]
	if !ok {
		return zero, fmt.Errorf("unsupported send kind %q", req.Kind)
	}
	if err := validateDelegateRequest(req); err != nil {
		return zero, err
	}
	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		resp, delegated, derr := tryDelegateSend(ctx, flags, err, req)
		if !delegated {
			return zero, err
		}
		if derr != nil {
			return zero, derr
		}
		var out T
		if len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, &out); err != nil {
				return zero, fmt.Errorf("decode the sync daemon's answer: %w", err)
			}
		}
		applySendMeta(&out, resp)
		return out, nil
	}
	defer closeApp(a, lk)
	if err := a.EnsureAuthed(ctx); err != nil {
		return zero, err
	}
	if resp, done, err := alreadySentResponse(ctx, a, req); err != nil {
		return zero, err
	} else if done {
		var cached T
		if len(resp.Data) > 0 {
			_ = json.Unmarshal(resp.Data, &cached)
		}
		applySendMeta(&cached, resp)
		return cached, nil
	}
	finish, early, err := beginIDSend(a, req)
	if err != nil {
		return zero, err
	}
	if early != nil {
		return zero, errors.New(early.Error)
	}
	res, err := h.run(ctx, a, req)
	finish(err)
	if err != nil {
		return zero, err
	}
	out, ok := res.(T)
	if !ok {
		return zero, fmt.Errorf("internal: %s returned %T", req.Kind, res)
	}
	return out, nil
}

// requireText returns an error naming flag when value is blank.
func requireText(flag, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", flag)
	}
	return nil
}

// validateTargetMessageID checks the ID of an existing message. These IDs come
// from other apps, so the rule is loose: printable, no spaces, at most 128.
func validateTargetMessageID(flag, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%s is required", flag)
	}
	if len(id) > 128 || strings.ContainsAny(id, " \t\r\n\x00") {
		return fmt.Errorf("invalid %s %q", flag, id)
	}
	return nil
}
