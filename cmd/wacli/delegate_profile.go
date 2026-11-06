package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/openclaw/wacli/internal/app"
)

const (
	profileSetNameKind       = "profile_set_name"
	profileSetAboutKind      = "profile_set_about"
	profileSetPictureKind    = "profile_set_picture"
	profileRemovePictureKind = "profile_remove_picture"
	profileGetAboutKind      = "profile_get_about"
	profilePictureInfoKind   = "profile_picture_info"
	profileBusinessKind      = "profile_business"
)

// profileMaxPictureBytes bounds the JPEG that travels over the socket. The
// client re-encodes to at most 640 px, so a real picture is far smaller.
const profileMaxPictureBytes = 4 * 1024 * 1024

type profileNameResult struct {
	Name string `json:"name"`
}

type profileAboutSetResult struct {
	About string `json:"about"`
}

type profilePictureSetResult struct {
	PictureID string `json:"picture_id"`
	Removed   bool   `json:"removed,omitempty"`
}

func init() {
	registerKind(profileSetNameKind, kindHandler{validate: func(r sendDelegateRequest) error {
		return validateProfileText("profile name", r.Name, 25)
	}, run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
		name := strings.TrimSpace(r.Name)
		if err := ensureLive(ctx, a); err != nil {
			return nil, err
		}
		if err := a.WA().SetProfileName(ctx, name); err != nil {
			return nil, fmt.Errorf("set profile name: %w", err)
		}
		return profileNameResult{Name: name}, nil
	}})
	registerKind(profileSetAboutKind, kindHandler{validate: func(r sendDelegateRequest) error {
		return validateProfileText("about text", r.About, 139)
	}, run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
		about := strings.TrimSpace(r.About)
		if err := ensureLive(ctx, a); err != nil {
			return nil, err
		}
		if err := a.WA().SetStatusMessage(ctx, about); err != nil {
			return nil, fmt.Errorf("set profile about: %w", err)
		}
		return profileAboutSetResult{About: about}, nil
	}})
	registerKind(profileSetPictureKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if len(r.Picture) == 0 {
			return fmt.Errorf("picture is required")
		}
		if len(r.Picture) > profileMaxPictureBytes {
			return fmt.Errorf("picture is larger than %d bytes", profileMaxPictureBytes)
		}
		if len(r.Picture) < 3 || r.Picture[0] != 0xFF || r.Picture[1] != 0xD8 {
			return fmt.Errorf("picture must be a JPEG")
		}
		return nil
	}, run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
		if err := ensureLive(ctx, a); err != nil {
			return nil, err
		}
		id, err := a.WA().SetProfilePicture(ctx, r.Picture)
		if err != nil {
			return nil, fmt.Errorf("set profile picture: %w", err)
		}
		return profilePictureSetResult{PictureID: id}, nil
	}})
	registerKind(profileRemovePictureKind, kindHandler{validate: func(sendDelegateRequest) error { return nil },
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			id, err := a.WA().SetProfilePicture(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("remove profile picture: %w", err)
			}
			return profilePictureSetResult{PictureID: id, Removed: true}, nil
		}})
	registerKind(profileGetAboutKind, kindHandler{skipsQueue: true, validate: validateProfileTarget,
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			target, _ := parseProfileTarget(r.To)
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			out, err := fetchProfileAbout(ctx, a.WA(), target)
			if err != nil {
				return nil, fmt.Errorf("get profile about: %w", err)
			}
			return out, nil
		}})
	registerKind(profilePictureInfoKind, kindHandler{skipsQueue: true, validate: func(r sendDelegateRequest) error {
		if err := validateProfileTarget(r); err != nil {
			return err
		}
		if len(r.ExistingID) > 128 || strings.ContainsAny(r.ExistingID, " \t\r\n") {
			return fmt.Errorf("invalid --existing-id")
		}
		return nil
	}, run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
		target, _ := parseProfileTarget(r.To)
		if err := ensureLive(ctx, a); err != nil {
			return nil, err
		}
		info, err := a.WA().GetProfilePictureInfo(ctx, target, r.Preview, r.ExistingID)
		if err != nil {
			return nil, fmt.Errorf("get profile picture info: %w", err)
		}
		return formatProfilePictureInfo(target, info), nil
	}})
	registerKind(profileBusinessKind, kindHandler{skipsQueue: true, validate: validateProfileTarget,
		run: func(ctx context.Context, a *app.App, r sendDelegateRequest) (any, error) {
			target, _ := parseProfileTarget(r.To)
			if err := ensureLive(ctx, a); err != nil {
				return nil, err
			}
			profile, err := a.WA().GetBusinessProfile(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("get business profile: %w", err)
			}
			return formatBusinessProfile(profile), nil
		}})
}

func validateProfileTarget(r sendDelegateRequest) error {
	_, err := parseProfileTarget(r.To)
	return err
}

func validateProfileText(what, value string, max int) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", what)
	}
	if n := len([]rune(value)); n > max {
		return fmt.Errorf("%s is %d characters; WhatsApp allows %d", what, n, max)
	}
	return nil
}
