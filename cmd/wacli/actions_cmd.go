package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

// The commands in this file are the CLI face of the account-action kinds in delegate_actions.go. Each one is a
// callKind: it runs on the sync daemon, or is refused on a pinned store with no daemon. None connects by itself.

// readOnlyActionKinds are the kinds that only read the account: they are allowed under --read-only.
var readOnlyActionKinds = map[string]bool{blocklistKind: true, privacyGetKind: true, statusAudienceKind: true}

func runActionKind(flags *rootFlags, req sendDelegateRequest, text string) error {
	if !readOnlyActionKinds[req.Kind] {
		if err := flags.requireWritable(); err != nil {
			return err
		}
	}
	ctx, cancel := withTimeout(context.Background(), flags)
	defer cancel()
	res, err := callKind[actionAck](ctx, flags, req)
	if err != nil {
		return err
	}
	if flags.asJSON {
		body := map[string]any{"ok": true}
		for k, v := range map[string]any{
			"chat": res.Chat, "jid": res.JID, "target": res.Target, "id": res.ID, "duration": res.Duration,
			"name": res.Name, "picture_id": res.PictureID,
		} {
			if v != "" {
				body[k] = v
			}
		}
		for k, v := range map[string]*bool{"starred": res.Starred, "pinned": res.Pinned, "blocked": res.Blocked,
			"muted": res.Muted, "deleted": res.Deleted, "rejected": res.Rejected} {
			if v != nil {
				body[k] = *v
			}
		}
		if res.JIDs != nil {
			body["jids"] = res.JIDs
		}
		if res.Settings != nil {
			body["settings"] = res.Settings
		}
		addSendMetaFields(body, res.sendMeta)
		return out.WriteJSON(os.Stdout, body)
	}
	switch {
	case req.Kind == statusAudienceKind && res.Audience != nil:
		fmt.Fprintf(os.Stdout, "%s: %d people\n", res.Audience.Type, res.Audience.Count)
	case res.JIDs != nil:
		for _, j := range res.JIDs {
			fmt.Fprintln(os.Stdout, j)
		}
		if len(res.JIDs) == 0 {
			fmt.Fprintln(os.Stdout, "No blocked contacts.")
		}
	case req.Kind == privacyGetKind || req.Kind == privacySetKind:
		if m, ok := res.Settings.(map[string]any); ok {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(os.Stdout, "%s: %v\n", k, m[k])
			}
		}
	default:
		fmt.Fprintln(os.Stdout, text)
	}
	return nil
}

func enableFlag(b bool) *bool { return &b }

func messageToggleCmd(flags *rootFlags, use, short, kind string, enable bool, withDuration bool) *cobra.Command {
	var chat, id, duration, newID string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--chat and --id are required")
			}
			if err := validateNewMessageID(newID); err != nil {
				return err
			}
			req := sendDelegateRequest{Kind: kind, Chat: chat, ID: id, Enable: enableFlag(enable), NewID: newID}
			if withDuration && enable {
				req.EphemeralDuration = duration
			}
			return runActionKind(flags, req, "OK")
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&id, "id", "", "message ID")
	if kind == messagePinKind {
		cmd.Flags().StringVar(&newID, "new-id", "", "caller-chosen ID for the pin message (8-64 letters or digits)")
	}
	if withDuration {
		cmd.Flags().StringVar(&duration, "duration", "7d", "how long the pin stays: 24h, 7d or 30d")
	}
	return cmd
}

func newMessagesStarCmd(flags *rootFlags, star bool) *cobra.Command {
	use, short := "star", "Star a message"
	if !star {
		use, short = "unstar", "Remove the star from a message"
	}
	return messageToggleCmd(flags, use, short, messageStarKind, star, false)
}

func newMessagesPinCmd(flags *rootFlags, pin bool) *cobra.Command {
	use, short := "pin", "Pin a message in its chat for everyone"
	if !pin {
		use, short = "unpin", "Unpin a message in its chat"
	}
	return messageToggleCmd(flags, use, short, messagePinKind, pin, true)
}

func newMessagesStatusAudienceCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status-audience",
		Short: "Show who a status post would reach (type and count)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionKind(flags, sendDelegateRequest{Kind: statusAudienceKind}, "OK")
		},
	}
}

func newMessagesStatusDeleteCmd(flags *rootFlags) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "status-delete",
		Short: "Delete one of your status posts",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: statusDeleteKind, ID: id}, "OK")
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "status message ID")
	return cmd
}

func newContactsBlockCmd(flags *rootFlags, block bool) *cobra.Command {
	var jid string
	use, short, action := "block", "Block a contact", "block"
	if !block {
		use, short, action = "unblock", "Unblock a contact", "unblock"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jid) == "" {
				return fmt.Errorf("--jid is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: contactBlockKind, To: jid, SubAction: action}, "OK")
		},
	}
	cmd.Flags().StringVar(&jid, "jid", "", "contact JID or phone number")
	return cmd
}

func newContactsBlocklistCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "blocklist",
		Short: "List blocked contacts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionKind(flags, sendDelegateRequest{Kind: blocklistKind}, "OK")
		},
	}
}

func newGroupsPhotoCmd(flags *rootFlags) *cobra.Command {
	var jid, file string
	var remove bool
	cmd := &cobra.Command{
		Use:   "photo",
		Short: "Set or remove a group picture (JPEG)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jid) == "" {
				return fmt.Errorf("--jid is required")
			}
			if remove == (strings.TrimSpace(file) != "") {
				return fmt.Errorf("exactly one of --file or --remove is required")
			}
			req := sendDelegateRequest{Kind: groupPhotoKind, GroupJID: jid}
			if remove {
				req.SubAction = "remove"
			} else {
				data, err := readPictureFile(file)
				if err != nil {
					return err
				}
				req.Picture = data
			}
			return runActionKind(flags, req, "OK")
		},
	}
	cmd.Flags().StringVar(&jid, "jid", "", "group JID (…@g.us)")
	cmd.Flags().StringVar(&file, "file", "", "JPEG file")
	cmd.Flags().BoolVar(&remove, "remove", false, "remove the picture")
	return cmd
}

// readPictureFile reads a group picture, refusing anything over the picture limit before it reads the file.
func readPictureFile(path string) ([]byte, error) {
	return readRegularFileLimitedWithOpener(path, profileMaxPictureBytes, openOutboundMedia)
}

func newGroupsJoinApprovalCmd(flags *rootFlags) *cobra.Command {
	return newGroupsExtraToggleCmd(flags, "join-approval", "Set whether new members need admin approval")
}

func newGroupsMemberAddCmd(flags *rootFlags) *cobra.Command {
	return newGroupsExtraToggleCmd(flags, "member-add", "Set who can add members (--on every member, --off admins only)")
}

func newGroupsExtraToggleCmd(flags *rootFlags, use, short string) *cobra.Command {
	var jid string
	var on, off bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jid) == "" {
				return fmt.Errorf("--jid is required")
			}
			enabled, err := parseOnOffFlags(cmd, on, off)
			if err != nil {
				return err
			}
			return runActionKind(flags, sendDelegateRequest{Kind: groupSettingKind, GroupJID: jid, SubAction: use, Enable: &enabled}, "OK")
		},
	}
	cmd.Flags().StringVar(&jid, "jid", "", "group JID (…@g.us)")
	cmd.Flags().BoolVar(&on, "on", false, "enable")
	cmd.Flags().BoolVar(&off, "off", false, "disable")
	return cmd
}

func newChatsDisappearingCmd(flags *rootFlags) *cobra.Command {
	var chat, duration string
	cmd := &cobra.Command{
		Use:   "disappearing",
		Short: "Set the disappearing-messages timer of one chat",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" || strings.TrimSpace(duration) == "" {
				return fmt.Errorf("--chat and --duration are required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: chatDisappearingKind, Chat: chat, EphemeralDuration: duration}, "OK")
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().StringVar(&duration, "duration", "", "off, 24h, 7d or 90d")
	return cmd
}

func newChatsDisappearingDefaultCmd(flags *rootFlags) *cobra.Command {
	var duration string
	cmd := &cobra.Command{
		Use:   "disappearing-default",
		Short: "Set the default disappearing-messages timer for new chats",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(duration) == "" {
				return fmt.Errorf("--duration is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: disappearingDefaultKd, EphemeralDuration: duration}, "OK")
		},
	}
	cmd.Flags().StringVar(&duration, "duration", "", "off, 24h, 7d or 90d")
	return cmd
}

func newChatsDeleteCmd(flags *rootFlags) *cobra.Command {
	var chat string
	var deleteMedia bool
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a chat on the phone and every linked device (not undoable)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(chat) == "" {
				return fmt.Errorf("--chat is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: chatDeleteKind, Chat: chat, DeleteMedia: deleteMedia}, "OK")
		},
	}
	cmd.Flags().StringVar(&chat, "chat", "", "chat JID or phone number")
	cmd.Flags().BoolVar(&deleteMedia, "delete-media", false, "also delete the chat's media on the phone")
	return cmd
}

func newChannelsMuteCmd(flags *rootFlags, mute bool) *cobra.Command {
	var jid string
	use, short := "mute", "Mute a channel"
	if !mute {
		use, short = "unmute", "Unmute a channel"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jid) == "" {
				return fmt.Errorf("--jid is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: channelMuteKind, To: jid, Enable: enableFlag(mute)}, "OK")
		},
	}
	cmd.Flags().StringVar(&jid, "jid", "", "channel JID (...@newsletter)")
	return cmd
}

func newChannelsCreateCmd(flags *rootFlags) *cobra.Command {
	var name, about string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a channel",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: channelCreateKind, Name: name, About: about}, "OK")
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "channel name")
	cmd.Flags().StringVar(&about, "about", "", "channel description")
	return cmd
}

func newPrivacyCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "privacy", Short: "Read or change the account's privacy settings"}
	cmd.AddCommand(&cobra.Command{
		Use:   "get",
		Short: "Show the privacy settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runActionKind(flags, sendDelegateRequest{Kind: privacyGetKind}, "OK")
		},
	})
	var name, value string
	set := &cobra.Command{
		Use:   "set",
		Short: "Change one privacy setting",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
				return fmt.Errorf("--name and --value are required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: privacySetKind, Name: name, Label: value}, "OK")
		},
	}
	set.Flags().StringVar(&name, "name", "", "groupadd, last, status (the About text visibility, NOT who sees your status posts), profile, readreceipts, online, calladd, messages, defense or stickers")
	set.Flags().StringVar(&value, "value", "", "all, contacts, contact_blacklist, none, match_last_seen, known, on_standard, off or contact_allowlist, as the setting allows")
	cmd.AddCommand(set)
	return cmd
}

func newCallsRejectCmd(flags *rootFlags) *cobra.Command {
	var jid, id string
	cmd := &cobra.Command{
		Use:   "reject",
		Short: "Reject an incoming call",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jid) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("--jid and --id are required")
			}
			return runActionKind(flags, sendDelegateRequest{Kind: callRejectKind, To: jid, ID: id}, "OK")
		},
	}
	cmd.Flags().StringVar(&jid, "jid", "", "caller JID")
	cmd.Flags().StringVar(&id, "id", "", "call ID")
	return cmd
}
