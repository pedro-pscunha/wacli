package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ImportResult counts what ImportFrom copied, or would copy in a dry run.
type ImportResult struct {
	Chats     int `json:"chats"`
	Messages  int `json:"messages"`
	Locations int `json:"locations"`
	// Messages this store already held: they stay exactly as they are here.
	Existing int  `json:"existing"`
	DryRun   bool `json:"dry_run"`
}

// ImportFrom copies from another wacli store of the same account the chats and messages that this store lacks,
// with the places of the copied messages. Use it when an old device of the account holds history that this
// device never received: a phone retry or a search needs the message in this store.
//
// It adds rows only. A chat or a message that this store already holds is never changed, so the import is safe to
// run twice (the second run copies nothing). The source opens read-only and is never migrated. A local media path
// is not copied: it names a file on the other device. Everything is one transaction; a dry run rolls it back.
func (d *DB) ImportFrom(ctx context.Context, sourcePath string, dryRun bool) (ImportResult, error) {
	result := ImportResult{DryRun: dryRun}
	if err := d.refuseSameFile(sourcePath); err != nil {
		return result, err
	}
	src, err := OpenReadOnly(sourcePath)
	if err != nil {
		return result, fmt.Errorf("open source store: %w", err)
	}
	defer src.Close()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	// The store's own writers, bound to this transaction, so a copied row gets the same columns, triggers and
	// search index as a row that sync wrote.
	target := &DB{path: d.path, sql: d.sql, q: d.q.WithTx(tx), ftsEnabled: d.ftsEnabled, receiptsEnabled: d.receiptsEnabled}

	if result.Chats, err = importChats(ctx, src, tx, target); err != nil {
		return result, err
	}
	copied, existing, err := importMessages(ctx, src, tx, target)
	if err != nil {
		return result, err
	}
	result.Messages, result.Existing = len(copied), existing
	if result.Locations, err = importLocations(ctx, src, target, copied); err != nil {
		return result, err
	}
	if dryRun {
		return result, nil
	}
	return result, tx.Commit()
}

func (d *DB) refuseSameFile(sourcePath string) error {
	source, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("source store: %w", err)
	}
	own, err := os.Stat(d.path)
	if err != nil {
		return err
	}
	if os.SameFile(source, own) {
		return errors.New("the source is this store itself")
	}
	return nil
}

func importChats(ctx context.Context, src *DB, tx *sql.Tx, target *DB) (int, error) {
	rows, err := src.sql.QueryContext(ctx, `SELECT jid, kind, COALESCE(name,''), COALESCE(last_message_ts,0) FROM chats`)
	if err != nil {
		return 0, fmt.Errorf("read source chats: %w", err)
	}
	defer rows.Close()
	copied := 0
	for rows.Next() {
		var jid, kind, name string
		var last int64
		if err := rows.Scan(&jid, &kind, &name, &last); err != nil {
			return copied, err
		}
		held, err := exists(ctx, tx, `SELECT 1 FROM chats WHERE jid = ?`, jid)
		if err != nil {
			return copied, err
		}
		if held {
			continue
		}
		var lastTS time.Time
		if last > 0 {
			lastTS = time.Unix(last, 0).UTC()
		}
		if err := target.UpsertChat(jid, kind, name, lastTS); err != nil {
			return copied, fmt.Errorf("copy chat %s: %w", jid, err)
		}
		copied++
	}
	return copied, rows.Err()
}

type messageKey struct{ chat, id string }

func importMessages(ctx context.Context, src *DB, tx *sql.Tx, target *DB) ([]messageKey, int, error) {
	rows, err := src.sql.QueryContext(ctx, `SELECT chat_jid, COALESCE(chat_name,''), msg_id, COALESCE(sender_jid,''),
		COALESCE(sender_name,''), ts, from_me, COALESCE(text,''), COALESCE(display_text,''), COALESCE(quoted_msg_id,''),
		COALESCE(quoted_sender_jid,''), COALESCE(buttons,''), is_forwarded, forwarding_score, COALESCE(reaction_to_id,''),
		COALESCE(reaction_emoji,''), COALESCE(media_type,''), COALESCE(media_caption,''), COALESCE(filename,''),
		COALESCE(mime_type,''), COALESCE(direct_path,''), media_key, file_sha256, file_enc_sha256,
		COALESCE(file_length,0), edited, revoked, deleted_for_me, COALESCE(deleted_at,0), COALESCE(deletion_reason,'')
		FROM messages ORDER BY rowid`)
	if err != nil {
		return nil, 0, fmt.Errorf("read source messages: %w", err)
	}
	defer rows.Close()
	var copied []messageKey
	existing := 0
	for rows.Next() {
		var p UpsertMessageParams
		var ts, deletedAt, fileLength int64
		var fromMe, forwarded, forwardingScore, edited, revoked, deletedForMe int64
		var buttons string
		if err := rows.Scan(&p.ChatJID, &p.ChatName, &p.MsgID, &p.SenderJID, &p.SenderName, &ts, &fromMe, &p.Text,
			&p.DisplayText, &p.QuotedMsgID, &p.QuotedSenderJID, &buttons, &forwarded, &forwardingScore,
			&p.ReactionToID, &p.ReactionEmoji, &p.MediaType, &p.MediaCaption, &p.Filename, &p.MimeType,
			&p.DirectPath, &p.MediaKey, &p.FileSHA256, &p.FileEncSHA256, &fileLength, &edited, &revoked,
			&deletedForMe, &deletedAt, &p.DeletionReason); err != nil {
			return copied, existing, err
		}
		held, err := exists(ctx, tx, `SELECT 1 FROM messages WHERE chat_jid = ? AND msg_id = ?`, p.ChatJID, p.MsgID)
		if err != nil {
			return copied, existing, err
		}
		if held {
			existing++
			continue
		}
		if buttons != "" {
			if err := json.Unmarshal([]byte(buttons), &p.Buttons); err != nil {
				return copied, existing, fmt.Errorf("message %s/%s: read buttons: %w", p.ChatJID, p.MsgID, err)
			}
		}
		p.Timestamp = time.Unix(ts, 0).UTC()
		p.FromMe, p.IsForwarded, p.Edited = fromMe != 0, forwarded != 0, edited != 0
		p.Revoked, p.DeletedForMe = revoked != 0, deletedForMe != 0
		p.ForwardingScore = uint32(forwardingScore)
		if fileLength > 0 {
			p.FileLength = uint64(fileLength)
		}
		if deletedAt > 0 {
			p.DeletedAt = time.Unix(deletedAt, 0).UTC()
		}
		if err := target.UpsertMessage(p); err != nil {
			return copied, existing, fmt.Errorf("copy message %s/%s: %w", p.ChatJID, p.MsgID, err)
		}
		copied = append(copied, messageKey{p.ChatJID, p.MsgID})
	}
	return copied, existing, rows.Err()
}

// A source from before the place table holds no places: nothing to copy.
func importLocations(ctx context.Context, src *DB, target *DB, copied []messageKey) (int, error) {
	if len(copied) == 0 || !src.hasTable("message_locations") {
		return 0, nil
	}
	n := 0
	for _, key := range copied {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		loc, err := src.GetMessageLocation(key.chat, key.id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return n, fmt.Errorf("read source place %s/%s: %w", key.chat, key.id, err)
		}
		if err := target.UpsertMessageLocation(loc); err != nil {
			return n, fmt.Errorf("copy place %s/%s: %w", key.chat, key.id, err)
		}
		n++
	}
	return n, nil
}

func exists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
