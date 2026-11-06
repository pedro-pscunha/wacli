# accounts

Read when: using more than one WhatsApp account, choosing the active account, or migrating from manual `--store` directories.

`wacli accounts` manages named accounts. Each account is an isolated store directory with its own WhatsApp linked-device session, local mirror database, media files, and lock.

## Commands

```bash
wacli accounts list
wacli accounts add NAME [--no-auth]
wacli accounts use NAME
wacli accounts show NAME
wacli accounts remove NAME
```

Use a named account with any command:

```bash
wacli --account work chats list
wacli --account personal send text --to 1234567890 --message "hi"
```

## Config

The default config path is `<base>/config.yaml`, where `<base>` is the default store root (`~/.wacli` on macOS and existing legacy Linux installs, otherwise `~/.local/state/wacli` on Linux).

```yaml
default_account: personal

accounts:
  personal:
    store: accounts/personal
  work:
    store: accounts/work
```

Relative `store` paths resolve from the config directory. Absolute paths are allowed for custom layouts.

## Selection Rules

Store selection is intentionally explicit:

1. `--store DIR` uses that exact store and cannot be combined with `--account`.
2. `--account NAME` resolves `NAME` from `config.yaml`.
3. `WACLI_STORE_DIR` keeps its existing override behavior for scripts and one-off stores.
4. If `default_account` is set, commands use that account.
5. If `config.yaml` has accounts and no `default_account`, the command fails and names the accounts: pass `--account`. A host with more than one live account can drop `default_account` so that every call chooses one.
6. Otherwise (no `config.yaml`, or no accounts in it) existing single-store behavior remains: XDG state dir on Linux, or `~/.wacli` elsewhere.

Account names may contain letters, digits, `.`, `_`, and `-`, and must start with a letter or digit.

## Notes

- `accounts add NAME` creates the isolated store and then runs the normal auth/bootstrap flow for that account. Use `--no-auth` to only write config and create the store.
- Only the first account added becomes `default_account`. Adding an account to a config whose `default_account` was removed keeps it removed; `accounts use NAME` sets one.
- Locks are per account store, so `wacli --account personal sync --follow` and `wacli --account work chats list` do not block each other unless they share the same store path.
- Cross-account search or status should be explicit aggregate commands, not accidental shared database queries.
- Use `--store DIR` for one-off migration/debugging against an old manual store.
