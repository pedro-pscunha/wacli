package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/wa"
)

const (
	contactAliasKind = "contact_alias"
	contactTagKind   = "contact_tag"

	// maxContactLabelRunes bounds an alias or a tag: they are shown in lists and filters.
	maxContactLabelRunes = 100
)

// contactAckResult is the answer of both kinds. JID is the key the metadata was stored under (the canonical JID),
// Alias and Tag are the stored (trimmed) values.
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

// validateContactJID accepts a JID or a bare phone number. The handler canonicalizes both to the same stored
// JID (canonicalContactJID), so what is stored is never the raw input.
func validateContactJID(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("--jid is required")
	}
	if _, err := wa.ParseUserOrJID(raw); err != nil {
		return fmt.Errorf("invalid --jid %q: %w", raw, err)
	}
	return nil
}

// canonicalContactJID turns what the caller typed into the JID the contact rows use. A bare number becomes
// <digits>@s.whatsapp.net; types.ParseJID alone would read it as a server name and store the metadata under a
// key no contact has.
func canonicalContactJID(raw string) (string, error) {
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		return "", err
	}
	return canonicalCLIJID(jid).String(), nil
}

// validateContactLabel checks an alias or a tag. It must be valid UTF-8 (invalid bytes would be stored raw on the
// local path and as U+FFFD on the daemon path, two different values for one input), not blank, at most 100
// characters, and free of control (Cc), format (Cf: bidi overrides such as U+202E, zero-width U+200B, U+FEFF) and
// line or paragraph separator (Zl, Zp) characters. Labels appear in lists, filters and the gateway's read API,
// and those characters can reorder, hide or split what a reader sees.
func validateContactLabel(flag, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", flag)
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("%s is required", flag)
	}
	if n := utf8.RuneCountInString(v); n > maxContactLabelRunes {
		return fmt.Errorf("%s is %d characters; the limit is %d", flag, n, maxContactLabelRunes)
	}
	for _, r := range v {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("%s must not contain control, format or separator characters (found U+%04X)", flag, r)
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
	jid, err := canonicalContactJID(r.To)
	if err != nil {
		return nil, err
	}
	jids, err := contactMetadataJIDs(ctx, a, jid)
	if err != nil {
		return nil, err
	}
	if r.SubAction == "rm" {
		if err := a.DB().RemoveAlias(jids); err != nil {
			return nil, err
		}
		return contactAckResult{JID: jids[0], Removed: true}, nil
	}
	alias := strings.TrimSpace(r.Alias)
	if err := a.DB().SetAlias(jids, alias); err != nil {
		return nil, err
	}
	return contactAckResult{JID: jids[0], Alias: alias}, nil
}

func runContactTag(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
	jid, err := canonicalContactJID(r.To)
	if err != nil {
		return nil, err
	}
	jids, err := contactMetadataJIDs(ctx, a, jid)
	if err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(r.Tag)
	if r.SubAction == "rm" {
		if err := a.DB().RemoveTag(jids, tag); err != nil {
			return nil, err
		}
		return contactAckResult{JID: jids[0], Tag: tag, Removed: true}, nil
	}
	if err := a.DB().AddTag(jids, tag); err != nil {
		return nil, err
	}
	return contactAckResult{JID: jids[0], Tag: tag}, nil
}
