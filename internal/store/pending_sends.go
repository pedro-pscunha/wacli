package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Pending send states. A row exists from just before the wire send until the
// send is known to have finished.
const (
	// PendingSendInFlight: the send started and its outcome is not known.
	PendingSendInFlight = "in_flight"
	// PendingSendSent: the message went out but the local store write failed, so
	// no messages row proves it. A retry answers "already sent".
	PendingSendSent = "sent"
	// PendingSendDone: the message went out and is stored. The row only remembers
	// the recipient as the caller typed it and the chat it stored under, so a
	// retry can tell "same number again" from "another number". It never blocks:
	// the stored message answers first.
	PendingSendDone = "done"
)

// pendingSendRetention bounds how long a row can block a retry. A caller-chosen
// id older than this is treated as new. `pending-sends clear` releases one early.
const pendingSendRetention = 7 * 24 * time.Hour

// PendingSend is one recorded send.
type PendingSend struct {
	MsgID       string    `json:"msg_id"`
	ChatJID     string    `json:"chat_jid"`
	TypedTo     string    `json:"typed_to"`
	Kind        string    `json:"kind"`
	ContentHash string    `json:"content_hash"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
}

// ContentHash fingerprints what a send says: its kind and its text or caption.
// Two requests with the same id and the same hash are one message sent again;
// the same id with another hash is an id reused for something else.
func ContentHash(kind, text string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}

// BeginPendingSend records that a send with msgID is about to go on the wire. If
// a row already exists it returns that row and inserted=false: an earlier
// attempt started and never finished.
func (d *DB) BeginPendingSend(p PendingSend) (existing PendingSend, inserted bool, err error) {
	p.MsgID = strings.TrimSpace(p.MsgID)
	if p.MsgID == "" {
		return PendingSend{}, false, fmt.Errorf("message ID is required")
	}
	now := nowUTC()
	_, _ = d.sql.Exec(`DELETE FROM pending_sends WHERE created_at < ?`, now.Add(-pendingSendRetention).Unix())
	res, err := d.sql.Exec(`INSERT OR IGNORE INTO pending_sends(msg_id, chat_jid, typed_to, kind, content_hash, state, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		p.MsgID, p.ChatJID, p.TypedTo, p.Kind, p.ContentHash, PendingSendInFlight, now.Unix())
	if err != nil {
		return PendingSend{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		p.State, p.CreatedAt = PendingSendInFlight, now
		return p, true, nil
	}
	got, ok, err := d.GetPendingSend(p.MsgID)
	if err != nil {
		return PendingSend{}, false, err
	}
	if !ok { // deleted between the insert and the read: treat as free
		return d.BeginPendingSend(p)
	}
	return got, false, nil
}

// GetPendingSend reads one row.
func (d *DB) GetPendingSend(msgID string) (PendingSend, bool, error) {
	var ps PendingSend
	var ts int64
	err := d.sql.QueryRow(`SELECT msg_id, chat_jid, typed_to, kind, content_hash, state, created_at FROM pending_sends WHERE msg_id = ?`,
		strings.TrimSpace(msgID)).Scan(&ps.MsgID, &ps.ChatJID, &ps.TypedTo, &ps.Kind, &ps.ContentHash, &ps.State, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingSend{}, false, nil
	}
	if err != nil {
		return PendingSend{}, false, err
	}
	ps.CreatedAt = time.Unix(ts, 0).UTC()
	return ps, true, nil
}

// ListPendingSends returns every recorded send, oldest first.
func (d *DB) ListPendingSends() ([]PendingSend, error) {
	rows, err := d.sql.Query(`SELECT msg_id, chat_jid, typed_to, kind, content_hash, state, created_at FROM pending_sends ORDER BY created_at, msg_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSend
	for rows.Next() {
		var ps PendingSend
		var ts int64
		if err := rows.Scan(&ps.MsgID, &ps.ChatJID, &ps.TypedTo, &ps.Kind, &ps.ContentHash, &ps.State, &ts); err != nil {
			return nil, err
		}
		ps.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, ps)
	}
	return out, rows.Err()
}

// CompletePendingSend records a send that went out and was stored: the row stays
// as a record of the typed recipient (see PendingSendDone).
func (d *DB) CompletePendingSend(msgID, storedChat string) error {
	_, err := d.sql.Exec(`UPDATE pending_sends SET state = ?, chat_jid = CASE WHEN ? <> '' THEN ? ELSE chat_jid END WHERE msg_id = ?`,
		PendingSendDone, storedChat, storedChat, strings.TrimSpace(msgID))
	return err
}

// FinishPendingSend removes the row: the send ended and its outcome is known.
// It reports whether a row was removed.
func (d *DB) FinishPendingSend(msgID string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM pending_sends WHERE msg_id = ?`, strings.TrimSpace(msgID))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkPendingSendSent keeps the row but records that the message went out. It
// also records the chat the send really stored under.
func (d *DB) MarkPendingSendSent(msgID, storedChat string) error {
	_, err := d.sql.Exec(`UPDATE pending_sends SET state = ?, chat_jid = CASE WHEN ? <> '' THEN ? ELSE chat_jid END WHERE msg_id = ?`,
		PendingSendSent, storedChat, storedChat, strings.TrimSpace(msgID))
	return err
}

// OwnMessageRef is what a stored message of this account says about itself.
type OwnMessageRef struct {
	ChatJID   string
	MediaType string
	Text      string
}

// FindOwnMessageRef is FindOwnMessage with the stored kind and text, for the
// "same message again, or an id reused" check.
func (d *DB) FindOwnMessageRef(msgID string) (OwnMessageRef, bool, error) {
	msgID = strings.TrimSpace(msgID)
	if msgID == "" {
		return OwnMessageRef{}, false, fmt.Errorf("message ID is required")
	}
	var ref OwnMessageRef
	var media, text, caption sql.NullString
	err := d.sql.QueryRow(`SELECT chat_jid, media_type, text, media_caption FROM messages WHERE msg_id = ? AND from_me = 1 LIMIT 1`, msgID).
		Scan(&ref.ChatJID, &media, &text, &caption)
	if errors.Is(err, sql.ErrNoRows) {
		return OwnMessageRef{}, false, nil
	}
	if err != nil {
		return OwnMessageRef{}, false, err
	}
	ref.MediaType = media.String
	ref.Text = text.String
	if strings.TrimSpace(ref.Text) == "" {
		ref.Text = caption.String
	}
	return ref, true, nil
}
