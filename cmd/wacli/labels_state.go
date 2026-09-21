package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func newLabelsCreateCmd(flags *rootFlags) *cobra.Command {
	var name string
	var color int32
	var id string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a label",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			var delegate *sendDelegateRequest
			if id != "" {
				// Only delegable with an explicit id: picking the next free id
				// needs a snapshot read that the daemon would have to do itself.
				delegate = &sendDelegateRequest{Kind: "label_edit", LabelID: id, LabelName: name, LabelColor: color}
			}
			return runLabelWrite(flags, "create", delegate, func(ctx context.Context, a *app.App) (string, error) {
				labelID := strings.TrimSpace(id)
				if labelID == "" {
					next, err := nextLabelID(ctx, a)
					if err != nil {
						return "", err
					}
					labelID = next
				}
				return labelID, a.EditLabel(ctx, labelID, name, color, false)
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "label name")
	cmd.Flags().Int32Var(&color, "color", 0, "label color index (0-19)")
	cmd.Flags().StringVar(&id, "id", "", "label id (default: next free id)")
	return cmd
}

func newLabelsRenameCmd(flags *rootFlags) *cobra.Command {
	var id, name string
	var color int32
	cmd := &cobra.Command{
		Use:   "rename",
		Short: "Rename or recolor a label",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
				return fmt.Errorf("--id and --name are required")
			}
			return runLabelWrite(flags, "rename",
				&sendDelegateRequest{Kind: "label_edit", LabelID: id, LabelName: name, LabelColor: color},
				func(ctx context.Context, a *app.App) (string, error) {
					return id, a.EditLabel(ctx, id, name, color, false)
				})
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "label id")
	cmd.Flags().StringVar(&name, "name", "", "new label name")
	cmd.Flags().Int32Var(&color, "color", 0, "label color index (0-19)")
	return cmd
}

func newLabelsDeleteCmd(flags *rootFlags) *cobra.Command {
	var id, name string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a label",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return runLabelWrite(flags, "delete",
				&sendDelegateRequest{Kind: "label_edit", LabelID: id, LabelName: name, LabelDeleted: true},
				func(ctx context.Context, a *app.App) (string, error) {
					// WhatsApp deletes by setting the deleted flag on the edit
					// action, so the name still travels with the mutation.
					return id, a.EditLabel(ctx, id, name, 0, true)
				})
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "label id")
	cmd.Flags().StringVar(&name, "name", "", "current label name (sent with the delete)")
	return cmd
}

func newLabelsAttachCmd(flags *rootFlags, attach bool) *cobra.Command {
	use, short := "attach", "Attach a label to a chat"
	if !attach {
		use, short = "detach", "Detach a label from a chat"
	}
	var id, chat string
	var pick int
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(chat) == "" {
				return fmt.Errorf("--id and --chat are required")
			}
			return runLabelWrite(flags, use,
				&sendDelegateRequest{Kind: "label_chat", LabelID: id, To: chat, Pick: pick, Enable: &attach},
				func(ctx context.Context, a *app.App) (string, error) {
					jid, err := resolveRecipient(a, chat, recipientOptions{pick: pick, asJSON: flags.asJSON})
					if err != nil {
						return "", err
					}
					return jid.String(), a.LabelChat(ctx, jid, id, attach)
				})
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "label id")
	cmd.Flags().StringVar(&chat, "chat", "", "chat name, phone number, or JID")
	cmd.Flags().IntVar(&pick, "pick", 0, "choose match N when --chat is ambiguous")
	return cmd
}

// nextLabelID returns the smallest unused numeric label id.
func nextLabelID(ctx context.Context, a *app.App) (string, error) {
	mutations, err := a.ReadLabelMutations(ctx)
	if err != nil {
		return "", err
	}
	used := map[int]bool{}
	for _, r := range collectLabels(mutations) {
		if n, convErr := strconv.Atoi(r.ID); convErr == nil {
			used[n] = true
		}
	}
	for n := 1; ; n++ {
		if !used[n] {
			return strconv.Itoa(n), nil
		}
	}
}

// runLabelWrite runs the label write in process when the store lock is free,
// and hands it to the sync daemon over the send socket when the daemon holds
// the lock. Without the delegate the caller would have to stop the daemon.
func runLabelWrite(flags *rootFlags, action string, delegate *sendDelegateRequest, run func(context.Context, *app.App) (string, error)) error {
	if err := flags.requireWritable(); err != nil {
		return err
	}

	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		if delegate != nil {
			resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, *delegate)
			if delegated {
				if delegateErr != nil {
					return delegateErr
				}
				if flags.asJSON {
					return out.WriteJSON(os.Stdout, map[string]any{
						"ok":     true,
						"action": action,
						"target": resp.Target,
					})
				}
				fmt.Fprintf(os.Stdout, "%s: %s\n", action, resp.Target)
				return nil
			}
		}
		return err
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	removePersistenceHandler, err := a.AddChatStatePersistenceHandler(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Keep the handler active until the socket is closed, then let App.Close
		// drain every persistence task before it closes the local database.
		a.WA().Disconnect()
		removePersistenceHandler()
	}()
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}

	target, err := run(ctx, a)
	if err != nil {
		return err
	}

	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{
			"ok":     true,
			"action": action,
			"target": target,
		})
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n", action, target)
	return nil
}
