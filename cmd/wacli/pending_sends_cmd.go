package main

import (
	"context"
	"fmt"
	"os"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

// newPendingSendsCmd manages the rows that make a retry answer resultado_incerto.
// It is local maintenance: it reads and writes wacli.db and never connects.
func newPendingSendsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pending-sends",
		Short: "List or release sends whose result is unknown (resultado_incerto)",
		Long: "A send with --id leaves a row when its result is unknown (the ack wait was cut\n" +
			"after the message went out). A retry with the same --id then answers\n" +
			"resultado_incerto and does not send. Check the phone: if the message is there,\n" +
			"nothing more is needed; if it is not, send again with a NEW --id, or release\n" +
			"the old id here when you are sure it never left.",
	}
	cmd.AddCommand(newPendingSendsListCmd(flags), newPendingSendsClearCmd(flags))
	return cmd
}

func newPendingSendsListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the recorded sends",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newLocalApp(ctx, flags, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)
			rows, err := a.DB().ListPendingSends()
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"pending_sends": rows, "count": len(rows)})
			}
			for _, r := range rows {
				fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%s\n", r.MsgID, r.State, r.Kind, r.ChatJID, r.CreatedAt.Local().Format("2006-01-02 15:04:05"))
			}
			return nil
		},
	}
}

func newPendingSendsClearCmd(flags *rootFlags) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Release one id so it can be sent again",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireText("--id", id); err != nil {
				return err
			}
			if err := flags.requireWritable(); err != nil {
				return err
			}
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newLocalApp(ctx, flags, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)
			removed, err := a.DB().FinishPendingSend(id)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{"id": id, "cleared": removed})
			}
			if !removed {
				fmt.Fprintf(os.Stdout, "No pending send for %s.\n", id)
				return nil
			}
			fmt.Fprintf(os.Stdout, "Released %s. It can be sent again; confirm on the phone first that the message is not there.\n", id)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "the message id to release")
	return cmd
}
