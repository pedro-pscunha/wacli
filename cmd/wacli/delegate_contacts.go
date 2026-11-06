package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/wa"
)

const (
	contactAliasKind = "contact_alias"
	contactTagKind   = "contact_tag"

	// maxContactLabelRunes bounds an alias or a tag: they are shown in lists and filters.
	maxContactLabelRunes = 100
)

// contactAckResult is the answer of both kinds.
type contactAckResult struct {
	JID     string `json:"jid"`
	Alias   string `json:"alias,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Removed bool   `json:"removed,omitempty"`
}

func init() {
	// Local writes to the daemon's own store: they skip the send queue and the pacing, and
	// they never need the WhatsApp connection.
	registerKind(contactAliasKind, kindHandler{skipsQueue: true, localOK: true, validate: validateContactAlias, run: runContactAlias})
	registerKind(contactTagKind, kindHandler{skipsQueue: true, localOK: true, validate: validateContactTag, run: runContactTag})
}

func validateContactJID(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("--jid is required")
	}
	if _, err := wa.ParseUserOrJID(raw); err != nil {
		return fmt.Errorf("invalid --jid %q: %w", raw, err)
	}
	return nil
}

// validateContactLabel checks an alias or a tag: not blank, at most 100 characters, no control characters.
func validateContactLabel(flag, value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("%s is required", flag)
	}
	if n := len([]rune(v)); n > maxContactLabelRunes {
		return fmt.Errorf("%s is %d characters; the limit is %d", flag, n, maxContactLabelRunes)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters", flag)
		}
	}
	return nil
}

func validateContactAlias(r sendDelegateRequest) error {
	if err := validateContactJID(r.To); err != nil {
		return err
	}
	switch r.SubAction {
	case "set":
		return validateContactLabel("--alias", r.Alias)
	case "rm":
		if strings.TrimSpace(r.Alias) != "" {
			return fmt.Errorf("--alias does not apply to rm")
		}
		return nil
	}
	return fmt.Errorf("unsupported alias action %q (set|rm)", r.SubAction)
}

func validateContactTag(r sendDelegateRequest) error {
	if err := validateContactJID(r.To); err != nil {
		return err
	}
	if r.SubAction != "add" && r.SubAction != "rm" {
		return fmt.Errorf("unsupported tag action %q (add|rm)", r.SubAction)
	}
	return validateContactLabel("--tag", r.Tag)
}

func runContactAlias(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jids, err := contactMetadataJIDs(ctx, a, r.To)
	if err != nil {
		return nil, err
	}
	if r.SubAction == "rm" {
		if err := a.DB().RemoveAlias(jids); err != nil {
			return nil, err
		}
		return contactAckResult{JID: r.To, Removed: true}, nil
	}
	alias := strings.TrimSpace(r.Alias)
	if err := a.DB().SetAlias(jids, alias); err != nil {
		return nil, err
	}
	return contactAckResult{JID: r.To, Alias: alias}, nil
}

func runContactTag(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jids, err := contactMetadataJIDs(ctx, a, r.To)
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(r.Tag)
	if r.SubAction == "rm" {
		if err := a.DB().RemoveTag(jids, tag); err != nil {
			return nil, err
		}
		return contactAckResult{JID: r.To, Tag: tag, Removed: true}, nil
	}
	if err := a.DB().AddTag(jids, tag); err != nil {
		return nil, err
	}
	return contactAckResult{JID: r.To, Tag: tag}, nil
}
