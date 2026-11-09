# store

Read when: inspecting local SQLite size/counts, pruning old local chat/group rows, or copying history from an old
store of the same account.

`wacli store` manages the selected account's local `wacli.db` mirror. Cleanup commands only delete local wacli cache/history rows; they do not delete WhatsApp chats, leave groups, or remove messages from WhatsApp servers.

## Commands

```bash
wacli store stats
wacli store cleanup [--days N] [--dry-run] [--confirm]
wacli store import --from PATH [--dry-run]
```

Related cleanup commands:

```bash
wacli chats cleanup [--days N] [--jid JID] [--dry-run] [--confirm]
wacli groups prune [--days N] [--left-only=false|--include-active] [--dry-run] [--confirm]
```

## Notes

- `store stats` reads local counts for chats, groups, left groups, and normal chat messages.
- Status broadcasts are persisted separately in `status_messages`; they are not chat rows and are not included in normal chat/message cleanup paths.
- What recipients report about your own messages is persisted in `message_receipts`, one row per recipient. The rows follow their chat through the SQLite cascade, so `store cleanup` and `chats cleanup` remove them with it.
- Location pins are persisted in `message_locations`. Coordinates follow the same lifecycle as the rest of a message's local payload: `messages purge` removes them with the retained payload, `store cleanup` and `chats cleanup` remove them with their chat, and a LID-to-phone identity migration moves them with the message rather than leaving them under the old identity.
- `store import --from PATH` copies from another store of the same account (an old device, a backup; PATH is its
  `wacli.db` or its store directory) the chats and messages that this store does not hold, with their places. Use it
  when an old device holds history that this device never received: this device can then search it, export it and
  ask the phone for its files.
  - It only adds rows, in one transaction: a chat or message that this store holds stays as it is, and a second run
    copies nothing. One bad source row fails the whole run, by name, and nothing is written.
  - Hidden-user (`@lid`) chat, sender and quoted-sender addresses are resolved with this account's own LID map first,
    as sync does, so a copied row is never a second copy of a message this store holds under the phone number.
    `unresolved_lid` counts copied messages whose chat stays `@lid`; the LID migration at a later sync start moves
    them when it learns the number.
  - Not copied: local media paths (they name files on the other device), a message that this store purged
    (`purged` counts them), polls, votes, stars, receipts and group metadata, and a chat's archive, pin, mute and
    unread state. A held chat keeps its name, kind and last activity. An edited message's edit time becomes its
    message time.
  - Nothing checks that the source belongs to this account (a logged-out store has no device): name the target with
    `--account`, and read `store` and `source` in the output.
  - Order: stop the store's sync daemon (the import is local maintenance and takes the store lock), back up
    `wacli.db` (`sqlite3 wacli.db ".backup <file>"`), run with `--dry-run`, run it, check
    `PRAGMA integrity_check`, start the daemon. Copy a live source the same way (`.backup` or `VACUUM INTO`): a copy
    without its `-wal` file loses the pages that the WAL still holds, with no error.
  - After an import, never run `media backfill` or `media retry` without `--chat` or `--limit`: every copied media row
    with an expired direct path counts as pending there, and a retry of all of them sends one receipt each to the
    phone. Ask for one message at a time with `media download --retry-on-phone`.
- `store cleanup` removes chats whose known local activity is older than `--days` and deletes their messages through the SQLite chat/message cascade.
- `chats cleanup --jid JID` removes one local chat row and its local messages.
- `groups prune` removes local group metadata plus the matching local chat/messages for pruned group JIDs.
- `groups prune` defaults to groups you have left. `--days N` limits that to groups left more than `N` days ago.
- `groups prune --include-active --days N` also prunes active groups whose last known local message is older than `N` days. Groups with no known local activity timestamp are skipped.
- Destructive cleanup commands require confirmation unless `--confirm` is passed.
- If a row cannot be deleted, bulk cleanup continues with the other targets, then exits nonzero with the underlying errors and the number successfully deleted. In `--json` mode, failures use the error envelope on stderr and do not emit a success result on stdout. Successfully deleted rows stay deleted; failed deletions are rolled back individually.
- Use `--dry-run` first; it lists what would be deleted without changing the local store.
- Use `--read-only` or `WACLI_READONLY=1` to make cleanup commands fail before opening the store for writes.
- Use `--account NAME` to target a named account store. Use `--store DIR` for manual stores or migration debugging; it cannot be combined with `--account`.

## Examples

```bash
wacli store stats
wacli store cleanup --days 365 --dry-run
wacli chats cleanup --jid 1234567890@s.whatsapp.net --dry-run
wacli groups prune --dry-run
wacli groups prune --days 180 --dry-run
wacli groups prune --include-active --days 365 --dry-run
```
