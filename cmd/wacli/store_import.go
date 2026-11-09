package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func newStoreImportCmd(flags *rootFlags) *cobra.Command {
	var from string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Copy the chats and messages that this store lacks from another store of the same account",
		Long: `Copy from another wacli store of the same account (an old device, a backup) the chats and messages that
this store does not hold, with their places. A message needs to be in this store before this device can ask the
phone for its file, search it or export it.

The import only adds rows: a chat or message that this store holds stays as it is, so a second run copies nothing.
Hidden-user (@lid) addresses are resolved with this account's own LID map first, as sync does; the target needs a
linked device, and session.db is opened (never connected) to read that map. The source opens
read-only. Local media paths are not copied, nor a message that this store purged. Nothing checks that the source
belongs to this account: name the target with --account. Stop the sync daemon of this store and back up its
wacli.db first: the import is local maintenance and takes the store lock. --from takes the source's wacli.db or its
store directory; copy a live source with sqlite's backup (or VACUUM INTO), never without its -wal file.

After an import, never run "media backfill" or "media retry" without --chat or --limit: every copied media row with
an expired path counts as pending there.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			source := strings.TrimSpace(from)
			if source == "" {
				return fmt.Errorf("--from is required")
			}
			if info, err := os.Stat(source); err == nil && info.IsDir() {
				source = filepath.Join(source, "wacli.db")
			}

			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newLocalApp(ctx, flags, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			result, err := a.ImportStore(ctx, source, dryRun)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, result)
			}
			verb := "Copied"
			if dryRun {
				verb = "Would copy"
			}
			fmt.Fprintf(os.Stdout, "From %s into %s:\n", result.Source, result.Store)
			fmt.Fprintf(os.Stdout, "%s %d chat(s), %d message(s) and %d place(s); %d message(s) were already here, %d purged here.\n",
				verb, result.Chats, result.Messages, result.Locations, result.Existing, result.Purged)
			if result.Unresolved > 0 {
				fmt.Fprintf(os.Stdout, "%d copied message(s) stay under an @lid chat: this account does not know its phone number yet.\n", result.Unresolved)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "the other store: its wacli.db or its store directory")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count what would be copied, write nothing")
	return cmd
}
