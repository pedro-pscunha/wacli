package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
)

const (
	capabilitiesKind = "capabilities"
	devicesKind      = "devices"
)

// delegateKinds lists every kind executeDelegatedSend accepts. The test
// TestDelegateKindsAreAllAccepted keeps it in step with the switch.
var delegateKinds = []string{
	"text", "file", "voice", "sticker", "react", "location", "poll", "poll_vote",
	"button_list_select", "presence", presenceSubscribeKind, "edit",
	markReadKind, markReadThroughKind, markReadReceiptsKind, contactsCheckKind,
	chatStateKind, labelChatKind, labelEditKind, mediaDownloadKind,
	capabilitiesKind, devicesKind,
}

// delegateCapabilities is the capabilities answer. Protocol is the socket
// version a client must send in `version`. Features name additive request
// fields: a client that needs one checks for it here before it relies on it.
type delegateCapabilities struct {
	Version  string   `json:"version"`
	Protocol int      `json:"protocol"`
	Kinds    []string `json:"kinds"`
	Features []string `json:"features"`
}

func currentCapabilities() *delegateCapabilities {
	kinds := append([]string(nil), delegateKinds...)
	sort.Strings(kinds)
	return &delegateCapabilities{
		Version:  effectiveVersion(),
		Protocol: sendDelegateVersion,
		Kinds:    kinds,
		Features: []string{"new_id", "delegate_only"},
	}
}

// isReadOnlyDelegateKind reports the kinds that only read.
func isReadOnlyDelegateKind(kind string) bool {
	return kind == capabilitiesKind || kind == devicesKind
}

func executeDelegatedCapabilities() (sendDelegateResponse, error) {
	return sendDelegateResponse{OK: true, Capabilities: currentCapabilities()}, nil
}

type delegatedDevicesApp interface {
	WA() app.WAClient
}

func executeDelegatedDevices(ctx context.Context, a delegatedDevicesApp) (sendDelegateResponse, error) {
	devices, err := a.WA().OwnDevices(ctx)
	if err != nil {
		return sendDelegateResponse{}, err
	}
	return sendDelegateResponse{OK: true, Devices: devices}, nil
}

func newCapabilitiesCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Show the version and delegate kinds of the running sync daemon",
		Long: "Ask the running `sync --follow` for its version, socket protocol and the\n" +
			"delegate kinds it accepts. It never connects to WhatsApp: with no daemon it\n" +
			"fails. Run it before you send a kind that an older daemon may not know.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			resp, err := delegateSend(ctx, flags, sendDelegateRequest{Kind: capabilitiesKind})
			if err != nil {
				if strings.Contains(err.Error(), errSendDelegateUnavailable.Error()) {
					return fmt.Errorf("daemon not running: %w", err)
				}
				return err
			}
			if resp.Capabilities == nil {
				return fmt.Errorf("the sync daemon answered without capabilities")
			}
			if flags.asJSON {
				return out.WriteJSON(os.Stdout, resp.Capabilities)
			}
			c := resp.Capabilities
			fmt.Fprintf(os.Stdout, "version %s, protocol %d\nkinds: %s\nfeatures: %s\n", c.Version, c.Protocol, strings.Join(c.Kinds, ", "), strings.Join(c.Features, ", "))
			return nil
		},
	}
}

func newDevicesCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List the devices linked to this account (read-only)",
		Long: "List the primary phone and every companion device linked to this account.\n" +
			"WhatsApp gives the device number; the platform and name are known for this\n" +
			"device only, and there is no last-seen time. While `sync --follow` runs, the\n" +
			"query uses its connection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()
			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				resp, delegated, delegateErr := tryDelegateSend(ctx, flags, err, sendDelegateRequest{Kind: devicesKind})
				if !delegated {
					return err
				}
				if delegateErr != nil {
					return delegateErr
				}
				return writeDevices(flags, resp.Devices)
			}
			defer closeApp(a, lk)
			if err := a.EnsureAuthed(ctx); err != nil {
				return err
			}
			if err := a.Connect(ctx, false, nil); err != nil {
				return err
			}
			list, err := a.WA().OwnDevices(ctx)
			if err != nil {
				return err
			}
			return writeDevices(flags, list)
		},
	}
}

func writeDevices(flags *rootFlags, devices []wa.OwnDevice) error {
	if flags.asJSON {
		return out.WriteJSON(os.Stdout, map[string]any{"devices": devices, "count": len(devices)})
	}
	for _, d := range devices {
		line := fmt.Sprintf("device %d  %s", d.Device, d.JID)
		if d.Self {
			line += "  (this device)"
		}
		if d.Platform != "" || d.Name != "" {
			line += fmt.Sprintf("  %s %s", d.Platform, d.Name)
		}
		fmt.Fprintln(os.Stdout, line)
	}
	return nil
}
