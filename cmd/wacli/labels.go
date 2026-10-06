package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

func newLabelsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "labels",
		Short: "Manage WhatsApp labels and the chats they are attached to",
	}
	cmd.AddCommand(newLabelsListCmd(flags))
	cmd.AddCommand(newLabelsCreateCmd(flags))
	cmd.AddCommand(newLabelsRenameCmd(flags))
	cmd.AddCommand(newLabelsDeleteCmd(flags))
	cmd.AddCommand(newLabelsAttachCmd(flags, true))
	cmd.AddCommand(newLabelsAttachCmd(flags, false))
	return cmd
}

func newLabelsListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List labels and their chats",
		Long: "Read the labels from WhatsApp app state and show which chats carry each one.\n" +
			"Connects with the account session and downloads the regular app state snapshot.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLabelsList(flags)
		},
	}
}

type labelChat struct {
	// JID is the preferred form of the membership: the LID when WhatsApp keyed
	// it on the contact's LID identity, the phone JID otherwise.
	JID string `json:"jid"`
	// JIDs carries every raw form the snapshot holds for this chat. One person
	// can appear under both the LID and the phone JID when an older client
	// wrote the phone form, and a phone-form leftover is exactly what a repair
	// has to find. So the merge keeps both instead of dropping one.
	JIDs     []string `json:"jids,omitempty"`
	PhoneJID string   `json:"phone_jid,omitempty"`
	Name     string   `json:"name,omitempty"`
}

type labelInfo struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Color   int32       `json:"color"`
	Deleted bool        `json:"deleted"`
	Chats   []labelChat `json:"chats"`
}

// collectLabels folds the app state snapshot into one row per label. A snapshot
// carries the current value of each index, so the last mutation for a key wins.
func collectLabels(mutations []appstate.Mutation) []labelInfo {
	type editState struct {
		name    string
		color   int32
		deleted bool
	}

	edits := map[string]editState{}
	assoc := map[string]map[string]bool{}

	for _, m := range mutations {
		if m.Action == nil || len(m.Index) == 0 {
			continue
		}
		switch m.Index[0] {
		case appstate.IndexLabelEdit:
			if len(m.Index) < 2 {
				continue
			}
			act := m.Action.GetLabelEditAction()
			if act == nil {
				continue
			}
			edits[m.Index[1]] = editState{
				name:    act.GetName(),
				color:   act.GetColor(),
				deleted: act.GetDeleted(),
			}
		case appstate.IndexLabelAssociationChat:
			if len(m.Index) < 3 {
				continue
			}
			act := m.Action.GetLabelAssociationAction()
			if act == nil {
				continue
			}
			labelID, jid := m.Index[1], m.Index[2]
			if assoc[labelID] == nil {
				assoc[labelID] = map[string]bool{}
			}
			assoc[labelID][jid] = act.GetLabeled()
		}
	}

	// A chat can carry a label whose edit mutation is absent from the snapshot,
	// so seed the label set from both maps.
	ids := map[string]bool{}
	for id := range edits {
		ids[id] = true
	}
	for id := range assoc {
		ids[id] = true
	}

	rows := make([]labelInfo, 0, len(ids))
	for id := range ids {
		e := edits[id]
		jids := make([]string, 0, len(assoc[id]))
		for jid, labeled := range assoc[id] {
			if labeled {
				jids = append(jids, jid)
			}
		}
		sort.Strings(jids)
		chats := make([]labelChat, 0, len(jids))
		for _, j := range jids {
			chats = append(chats, labelChat{JID: j})
		}
		rows = append(rows, labelInfo{
			ID:      id,
			Name:    e.name,
			Color:   e.color,
			Deleted: e.deleted,
			Chats:   chats,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

// labelChatName returns the best name for one chat, trying the phone form of
// the JID before the raw form.
//
// Both rows can exist with different names, and the phone-JID row is the better
// one: on this account the LID rows held "." and "Higor" where the phone rows
// held "Caike Engenharia" and "Higor Safra". A chat row sometimes carries the
// JID itself as its name, which is not a name.
func labelChatName(a *app.App, raw, phone string) string {
	for _, jid := range []string{phone, raw} {
		if c, err := a.DB().GetContact(jid); err == nil {
			if name := strings.TrimSpace(c.Name); name != "" {
				return name
			}
		}
		if ch, err := a.DB().GetChat(jid); err == nil {
			if name := strings.TrimSpace(ch.Name); name != "" && name != jid {
				return name
			}
		}
	}
	return ""
}

// resolveLabelChatNames names every membership and merges the two forms of one
// chat into one row.
//
// Most memberships key on a <digits>@lid JID that no table in wacli.db knows.
// The session store maps that LID to the phone JID, and the name lookup then
// runs on the form the contact tables carry.
func resolveLabelChatNames(ctx context.Context, a *app.App, rows []labelInfo) {
	toPhone := func(lid types.JID) string {
		if pn := a.WA().ResolveLIDToPN(ctx, lid); pn.Server == types.DefaultUserServer {
			return pn.String()
		}
		return lid.String()
	}
	name := func(raw, phone string) string { return labelChatName(a, raw, phone) }
	for i := range rows {
		rows[i].Chats = mergeLabelChats(rows[i].Chats, toPhone, name)
	}
}

// mergeLabelChats folds the memberships of one label into one row per person.
// toPhone maps a LID to its phone JID, and name returns the display name.
func mergeLabelChats(chats []labelChat, toPhone func(types.JID) string, name func(raw, phone string) string) []labelChat {
	merged := make([]labelChat, 0, len(chats))
	index := map[string]int{}

	for _, c := range chats {
		raw := c.JID
		phone := raw
		isLID := false
		if jid, err := types.ParseJID(raw); err == nil && jid.Server == types.HiddenUserServer {
			isLID = true
			phone = toPhone(jid)
		}

		at, seen := index[phone]
		if !seen {
			merged = append(merged, labelChat{JID: raw, PhoneJID: phone})
			at = len(merged) - 1
			index[phone] = at
		}
		merged[at].JIDs = append(merged[at].JIDs, raw)
		if isLID {
			// The LID form is the one WhatsApp keys on, so it names the row.
			merged[at].JID = raw
		}
		if merged[at].Name == "" {
			merged[at].Name = name(raw, phone)
		}
	}

	for j := range merged {
		sort.Strings(merged[j].JIDs)
		if merged[j].PhoneJID == merged[j].JID {
			merged[j].PhoneJID = ""
		}
	}
	return merged
}

func runLabelsList(flags *rootFlags) error {
	if err := flags.requireWritable(); err != nil {
		return err
	}

	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()

	a, lk, err := newApp(ctx, flags, true, false)
	if err != nil {
		return err
	}
	defer closeApp(a, lk)

	if err := a.EnsureAuthed(ctx); err != nil {
		return err
	}
	if err := a.Connect(ctx, false, nil); err != nil {
		return err
	}

	mutations, err := a.ReadLabelMutations(ctx)
	if err != nil {
		return err
	}
	rows := collectLabels(mutations)
	resolveLabelChatNames(ctx, a, rows)

	if flags.asJSON {
		return out.WriteJSON(os.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "no labels found")
		return nil
	}
	for _, r := range rows {
		state := ""
		if r.Deleted {
			state = " (deleted)"
		}
		fmt.Fprintf(os.Stdout, "%s\tcolor=%d\tid=%s\tchats=%d%s\n", r.Name, r.Color, r.ID, len(r.Chats), state)
		for _, c := range r.Chats {
			name := c.Name
			if name == "" {
				name = "(unknown)"
			}
			jids := c.JID
			if len(c.JIDs) > 1 {
				jids = strings.Join(c.JIDs, " + ")
			}
			fmt.Fprintf(os.Stdout, "    %s\t%s\n", name, jids)
		}
	}
	return nil
}
