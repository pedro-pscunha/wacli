package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/lock"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

const sourceVersion = "0.22.0"

var version string

const releaseLinkerSettingPrefix = "wacli-release-linker-version=["

var releaseLinkerSetting string

func effectiveVersion() string {
	if version == "" && releaseLinkerSetting == "" {
		return sourceVersion
	}
	// Source/HEAD package builds historically set only main.version. Official
	// release artifacts additionally require the marker during verification.
	if version != "" && releaseLinkerSetting == "" {
		return version
	}
	if version != "" && releaseLinkerSetting == releaseLinkerSettingPrefix+version+"]" {
		return version
	}
	return "invalid-release-linker-version"
}

const docsURL = "https://wacli.sh"

type rootFlags struct {
	storeDir   string
	account    string
	asJSON     bool
	fullOutput bool
	events     bool
	timeout    time.Duration
	readOnly   bool
	lockWait   time.Duration
	// delegateOnly makes every command that would connect to WhatsApp use the
	// running sync daemon, and refuse when none runs.
	delegateOnly bool
	// isDaemon marks a process that runs the full sync with the store handler
	// installed: `sync --follow` and `auth` login. Such a process may connect,
	// because everything it receives is stored; the store lock still keeps it
	// from running beside the daemon. `auth logout` is not one of them.
	isDaemon bool
}

func execute(args []string) error {
	var flags rootFlags

	rootCmd := &cobra.Command{
		Use:           "wacli",
		Short:         "WhatsApp CLI: sync, search, send",
		Long:          "wacli is a WhatsApp CLI for syncing, searching, and sending from local scripts.\n\nDocs: " + docsURL,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       effectiveVersion(),
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			wa.SetLibsignalEvents(out.NewEventWriter(os.Stderr, flags.events))
		},
	}
	rootCmd.SetVersionTemplate("wacli {{.Version}}\n")

	rootCmd.PersistentFlags().StringVar(&flags.storeDir, "store", "", "store directory (default: $WACLI_STORE_DIR, XDG state dir on Linux, or ~/.wacli)")
	rootCmd.PersistentFlags().StringVar(&flags.account, "account", "", "named account from config.yaml")
	rootCmd.PersistentFlags().BoolVar(&flags.asJSON, "json", false, "output JSON instead of human-readable text")
	rootCmd.PersistentFlags().BoolVar(&flags.fullOutput, "full", false, "disable truncation in table output")
	rootCmd.PersistentFlags().BoolVar(&flags.events, "events", false, "emit machine-readable NDJSON lifecycle events on stderr")
	rootCmd.PersistentFlags().DurationVar(&flags.timeout, "timeout", 5*time.Minute, "command timeout (non-sync commands)")
	rootCmd.PersistentFlags().DurationVar(&flags.lockWait, "lock-wait", 0, "wait for the store lock before failing (write commands)")
	rootCmd.PersistentFlags().BoolVar(&flags.readOnly, "read-only", false, "reject commands that intentionally write WhatsApp or the local store (or set WACLI_READONLY=1)")

	rootCmd.PersistentFlags().BoolVar(&flags.delegateOnly, "delegate-only", false, "never connect to WhatsApp from this process: delegate to the running sync daemon or fail (or set WACLI_DELEGATE_ONLY=1)")

	rootCmd.AddCommand(newVersionCmd())
	rootCmd.AddCommand(newAccountsCmd(&flags))
	rootCmd.AddCommand(newDoctorCmd(&flags))
	rootCmd.AddCommand(newAuthCmd(&flags))
	rootCmd.AddCommand(newSyncCmd(&flags))
	rootCmd.AddCommand(newMessagesCmd(&flags))
	rootCmd.AddCommand(newCallsCmd(&flags))
	rootCmd.AddCommand(newSendCmd(&flags))
	rootCmd.AddCommand(newPollCmd(&flags))
	rootCmd.AddCommand(newPollsCmd(&flags))
	rootCmd.AddCommand(newMediaCmd(&flags))
	rootCmd.AddCommand(newContactsCmd(&flags))
	rootCmd.AddCommand(newChatsCmd(&flags))
	rootCmd.AddCommand(newGroupsCmd(&flags))
	rootCmd.AddCommand(newChannelsCmd(&flags))
	rootCmd.AddCommand(newLabelsCmd(&flags))
	rootCmd.AddCommand(newHistoryCmd(&flags))
	rootCmd.AddCommand(newPresenceCmd(&flags))
	rootCmd.AddCommand(newProfileCmd(&flags))
	rootCmd.AddCommand(newDocsCmd(&flags))
	rootCmd.AddCommand(newCapabilitiesCmd(&flags))
	rootCmd.AddCommand(newDevicesCmd(&flags))
	rootCmd.AddCommand(newStoreCmd(&flags))

	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		writeRootError(flags, err)
		return err
	}
	return nil
}

func writeRootError(flags rootFlags, err error) {
	if err == nil {
		return
	}
	if flags.events {
		_ = out.NewEventWriter(os.Stderr, true).Emit("error", map[string]any{"message": err.Error()})
		return
	}
	_ = out.WriteError(os.Stderr, flags.asJSON, err)
}

func newApp(ctx context.Context, flags *rootFlags, needLock bool, allowUnauthed bool) (*app.App, *lock.Lock, error) {
	storeDir, err := resolveStoreDir(flags)
	if err != nil {
		return nil, nil, err
	}

	delegateOnly := flags.delegateOnlyFor(storeDir)
	if needLock && delegateOnly {
		// Decide from the daemon's socket, before the lock or wacli.db are
		// touched: a refused call must not hold either, or it can block the
		// daemon's own restart. A listening daemon means the store is in use,
		// and the caller delegates; nothing listening means refuse.
		if !sendDelegateAvailable(ctx, storeDir) {
			return nil, nil, app.ErrDelegateOnly
		}
		return nil, nil, fmt.Errorf("%w: the sync daemon serves this store (--delegate-only)", lock.ErrLocked)
	}

	var lk *lock.Lock
	if needLock {
		lk, err = lock.AcquireWithTimeout(ctx, storeDir, flags.lockWait)
		if err != nil {
			return nil, nil, err
		}
	}

	a, err := app.New(app.Options{
		StoreDir:      storeDir,
		Version:       effectiveVersion(),
		JSON:          flags.asJSON,
		Events:        out.NewEventWriter(os.Stderr, flags.events),
		AllowUnauthed: allowUnauthed,
		ReadOnly:      flags.isReadOnly(),
		DelegateOnly:  delegateOnly,
	})
	if err != nil {
		if lk != nil {
			_ = lk.Release()
		}
		return nil, nil, err
	}

	return a, lk, nil
}

func resolveStoreDir(flags *rootFlags) (string, error) {
	storeDir := ""
	account := ""
	if flags != nil {
		storeDir = flags.storeDir
		account = strings.TrimSpace(flags.account)
	}
	if storeDir != "" && account != "" {
		return "", fmt.Errorf("--store and --account cannot be combined")
	}
	switch {
	case storeDir != "":
	case account != "":
		resolved, _, err := config.ResolveAccountStore(config.DefaultConfigPath(), account)
		if err != nil {
			return "", err
		}
		storeDir = resolved
	case os.Getenv(config.EnvStoreDir) != "":
		storeDir = config.DefaultStoreDir()
	default:
		cfg, found, err := config.LoadAccountsConfigIfExists(config.DefaultConfigPath())
		if err != nil {
			return "", err
		}
		if found && strings.TrimSpace(cfg.DefaultAccount) != "" {
			resolved, _, err := config.ResolveAccountStore(config.DefaultConfigPath(), cfg.DefaultAccount)
			if err != nil {
				return "", err
			}
			storeDir = resolved
		} else if found && len(cfg.Accounts) > 0 {
			// Accounts and no default_account: the account is a choice, never a fallback. The base store would be
			// an empty one, so a read would answer "nothing" for an account that has messages.
			names := make([]string, 0, len(cfg.Accounts))
			for name := range cfg.Accounts {
				names = append(names, name)
			}
			sort.Strings(names)
			return "", fmt.Errorf("no account selected and no default_account in %s: pass --account (one of: %s)",
				config.DefaultConfigPath(), strings.Join(names, ", "))
		} else {
			storeDir = config.DefaultStoreDir()
		}
	}
	storeDir, _ = filepath.Abs(storeDir)
	return storeDir, nil
}

func (f *rootFlags) isReadOnly() bool {
	if f == nil {
		return false
	}
	if f.readOnly {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WACLI_READONLY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (f *rootFlags) isDelegateOnly() bool {
	if f == nil || f.isDaemon {
		return false
	}
	if f.delegateOnly {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WACLI_DELEGATE_ONLY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// delegateOnlyFor reports whether this call is delegate-only for storeDir: the
// flag, the environment variable, or a per-store marker or account setting that
// no caller can forget. The sync daemon itself is never delegate-only.
func (f *rootFlags) delegateOnlyFor(storeDir string) bool {
	if f != nil && f.isDaemon {
		return false
	}
	if f.isDelegateOnly() {
		return true
	}
	return config.StoreIsDelegateOnly(storeDir, config.DefaultConfigPath())
}

func (f *rootFlags) requireWritable() error {
	if f.isReadOnly() {
		return fmt.Errorf("read-only mode: command would intentionally modify WhatsApp or the local store")
	}
	return nil
}

func withTimeout(ctx context.Context, flags *rootFlags) (context.Context, context.CancelFunc) {
	if flags.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, flags.timeout)
}

func closeApp(a *app.App, lk *lock.Lock) {
	if a != nil {
		a.Close()
	}
	if lk != nil {
		_ = lk.Release()
	}
}
