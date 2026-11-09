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
The source opens read-only. Local media paths are not copied. Stop the sync daemon of this store first: the import
is local maintenance and takes the store lock. --from takes the source's wacli.db or its store directory.`,
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

			result, err := a.DB().ImportFrom(ctx, source, dryRun)
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
			fmt.Fprintf(os.Stdout, "%s %d chat(s), %d message(s) and %d place(s); %d message(s) were already here.\n",
				verb, result.Chats, result.Messages, result.Locations, result.Existing)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "the other store: its wacli.db or its store directory")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count what would be copied, write nothing")
	return cmd
}
