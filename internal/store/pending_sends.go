package store

import (
	"database/sql"
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
	// no messages row proves it.
	PendingSendSent = "sent"
)

// pendingSendRetention bounds how long a row can block a retry. A caller-chosen
// id older than this is treated as new.
const pendingSendRetention = 7 * 24 * time.Hour

// BeginPendingSend records that a send with msgID is about to go on the wire. If
// a row already exists, it returns that row's state and inserted=false: an
// earlier attempt started and never finished.
func (d *DB) BeginPendingSend(msgID, chatJID string) (state string, inserted bool, err error) {
	msgID = strings.TrimSpace(msgID)
	if msgID == "" {
		return "", false, fmt.Errorf("message ID is required")
	}
	now := nowUTC()
	_, _ = d.sql.Exec(`DELETE FROM pending_sends WHERE created_at < ?`, now.Add(-pendingSendRetention).Unix())
	res, err := d.sql.Exec(`INSERT OR IGNORE INTO pending_sends(msg_id, chat_jid, state, created_at) VALUES(?, ?, ?, ?)`,
		msgID, chatJID, PendingSendInFlight, now.Unix())
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return PendingSendInFlight, true, nil
	}
	err = d.sql.QueryRow(`SELECT state FROM pending_sends WHERE msg_id = ?`, msgID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingSendInFlight, false, nil
	}
	return state, false, err
}

// FinishPendingSend removes the row: the send ended and its outcome is known.
func (d *DB) FinishPendingSend(msgID string) error {
	_, err := d.sql.Exec(`DELETE FROM pending_sends WHERE msg_id = ?`, strings.TrimSpace(msgID))
	return err
}

// MarkPendingSendSent keeps the row but records that the message went out.
func (d *DB) MarkPendingSendSent(msgID string) error {
	_, err := d.sql.Exec(`UPDATE pending_sends SET state = ? WHERE msg_id = ?`, PendingSendSent, strings.TrimSpace(msgID))
	return err
}
