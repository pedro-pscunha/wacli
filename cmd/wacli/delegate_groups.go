package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
)

const (
	groupCreateKind       = "group_create"
	groupRenameKind       = "group_rename"
	groupTopicKind        = "group_topic"
	groupSettingKind      = "group_setting"
	groupParticipantsKind = "group_participants"
	groupRequestsListKind = "group_requests_list"
	groupRequestsKind     = "group_requests"
	groupInviteKind       = "group_invite"
	groupJoinKind         = "group_join"
	groupLeaveKind        = "group_leave"
	groupInfoKind         = "group_info"
	groupRefreshKind      = "group_refresh"
)

type groupInfoResult struct {
	Info *types.GroupInfo `json:"info"`
}

type groupAckResult struct {
	JID     string `json:"jid"`
	Name    string `json:"name,omitempty"`
	Topic   string `json:"topic,omitempty"`
	Setting string `json:"setting,omitempty"`
	Enabled bool   `json:"enabled,omitempty"`
	Link    string `json:"link,omitempty"`
	Revoked bool   `json:"revoked,omitempty"`
	Joined  bool   `json:"joined,omitempty"`
	Left    bool   `json:"left,omitempty"`
}

type groupParticipantsResult struct {
	Updated []types.GroupParticipant `json:"updated"`
}

type groupRequestsListResult struct {
	Entries []requestListEntry `json:"entries"`
}

type groupRefreshResult struct {
	Groups int `json:"groups"`
}

func init() {
	registerKind(groupCreateKind, kindHandler{validate: validateGroupCreate, run: groupCreate})
	registerKind(groupRenameKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if err := validateGroupJID(r); err != nil {
			return err
		}
		return requireText("--name", r.Name)
	}, run: groupRename})
	registerKind(groupTopicKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if err := validateGroupJID(r); err != nil {
			return err
		}
		if r.Topic == nil {
			return fmt.Errorf("--text is required")
		}
		return nil
	}, run: groupTopic})
	registerKind(groupSettingKind, kindHandler{validate: validateGroupSetting, run: groupSetting})
	registerKind(groupParticipantsKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if err := validateGroupJID(r); err != nil {
			return err
		}
		switch wa.GroupParticipantAction(r.SubAction) {
		case wa.GroupParticipantAdd, wa.GroupParticipantRemove, wa.GroupParticipantPromote, wa.GroupParticipantDemote:
		default:
			return fmt.Errorf("unsupported participants action %q (add|remove|promote|demote)", r.SubAction)
		}
		return validateGroupUsers(r.Users)
	}, run: groupParticipants})
	registerKind(groupRequestsListKind, kindHandler{validate: validateGroupJID, run: groupRequestsList, skipsQueue: true})
	registerKind(groupRequestsKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if err := validateGroupJID(r); err != nil {
			return err
		}
		switch wa.GroupParticipantRequestAction(r.SubAction) {
		case wa.GroupParticipantRequestApprove, wa.GroupParticipantRequestReject:
		default:
			return fmt.Errorf("unsupported requests action %q (approve|reject)", r.SubAction)
		}
		return validateGroupUsers(r.Users)
	}, run: groupRequests})
	registerKind(groupInviteKind, kindHandler{validate: func(r sendDelegateRequest) error {
		if err := validateGroupJID(r); err != nil {
			return err
		}
		if r.SubAction != "get" && r.SubAction != "revoke" {
			return fmt.Errorf("unsupported invite action %q (get|revoke)", r.SubAction)
		}
		return nil
	}, run: groupInvite})
	registerKind(groupJoinKind, kindHandler{validate: func(r sendDelegateRequest) error {
		code := strings.TrimSpace(r.Code)
		if code == "" {
			return fmt.Errorf("--code is required")
		}
		if len(code) > 512 || strings.ContainsAny(code, " \t\r\n") {
			return fmt.Errorf("invalid --code")
		}
		return nil
	}, run: groupJoin})
	registerKind(groupLeaveKind, kindHandler{validate: validateGroupJID, run: groupLeave})
	registerKind(groupInfoKind, kindHandler{validate: validateGroupJID, run: groupInfo, skipsQueue: true})
	registerKind(groupRefreshKind, kindHandler{validate: func(sendDelegateRequest) error { return nil }, run: groupRefresh, skipsQueue: true})
}

func validateGroupJID(r sendDelegateRequest) error {
	if err := requireText("--jid", r.GroupJID); err != nil {
		return err
	}
	_, err := parseGroupJID(r.GroupJID)
	return err
}

func validateGroupUsers(users []string) error {
	if len(users) == 0 {
		return fmt.Errorf("at least one --user is required")
	}
	if len(users) > 256 {
		return fmt.Errorf("at most 256 --user values")
	}
	_, err := parseGroupUserJIDs(users)
	return err
}

func validateGroupCreate(r sendDelegateRequest) error {
	if err := requireText("--name", r.Name); err != nil {
		return err
	}
	if r.Parent && strings.TrimSpace(r.LinkedParent) != "" {
		return fmt.Errorf("--community and --linked-parent cannot be combined")
	}
	if strings.TrimSpace(r.LinkedParent) != "" {
		if _, err := parseGroupJID(r.LinkedParent); err != nil {
			return fmt.Errorf("parse --linked-parent: %w", err)
		}
	}
	if len(r.Users) > 0 {
		return validateGroupUsers(r.Users)
	}
	return nil
}

func validateGroupSetting(r sendDelegateRequest) error {
	if err := validateGroupJID(r); err != nil {
		return err
	}
	switch r.SubAction {
	case "announce-only", "locked", "join-approval", "member-add":
	default:
		return fmt.Errorf("unsupported group setting %q (announce-only|locked|join-approval|member-add)", r.SubAction)
	}
	if r.Enable == nil {
		return fmt.Errorf("exactly one of --on or --off is required")
	}
	return nil
}

func persistGroup(ctx context.Context, a *app.App, jid types.JID) {
	if info, err := a.WA().GetGroupInfo(ctx, jid); err == nil && info != nil {
		_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	}
}

func groupCreate(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	participants, err := parseGroupUserJIDs(req.Users)
	if err != nil {
		return nil, err
	}
	var parentJID types.JID
	if strings.TrimSpace(req.LinkedParent) != "" {
		if parentJID, err = parseGroupJID(req.LinkedParent); err != nil {
			return nil, fmt.Errorf("parse --linked-parent: %w", err)
		}
	}
	info, err := a.WA().CreateGroup(ctx, wa.CreateGroupRequest{
		Name:                   req.Name,
		Participants:           participants,
		IsAnnounce:             req.Announce,
		IsLocked:               req.Locked,
		IsJoinApprovalRequired: req.JoinApproval,
		IsParent:               req.Parent,
		LinkedParentJID:        parentJID,
	})
	if err != nil {
		return nil, err
	}
	if info != nil {
		_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	}
	return groupInfoResult{Info: info}, nil
}

func groupRename(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	if err := a.WA().SetGroupName(ctx, jid, req.Name); err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupAckResult{JID: jid.String(), Name: req.Name}, nil
}

func groupTopic(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	if err := a.WA().SetGroupTopic(ctx, jid, *req.Topic); err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupAckResult{JID: jid.String(), Topic: *req.Topic}, nil
}

func groupSetting(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	enabled := *req.Enable
	var err error
	switch req.SubAction {
	case "announce-only":
		err = a.WA().SetGroupAnnounce(ctx, jid, enabled)
	case "locked":
		err = a.WA().SetGroupLocked(ctx, jid, enabled)
	case "join-approval":
		err = a.WA().SetGroupJoinApprovalMode(ctx, jid, enabled)
	case "member-add":
		err = a.WA().SetGroupMemberAddMode(ctx, jid, enabled)
	}
	if err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupAckResult{JID: jid.String(), Setting: req.SubAction, Enabled: enabled}, nil
}

func groupParticipants(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	users, err := parseGroupUserJIDs(req.Users)
	if err != nil {
		return nil, err
	}
	updated, err := a.WA().UpdateGroupParticipants(ctx, jid, users, wa.GroupParticipantAction(req.SubAction))
	if err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupParticipantsResult{Updated: updated}, nil
}

func groupRequestsList(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	requests, err := a.WA().GetGroupRequestParticipants(ctx, jid)
	if err != nil {
		return nil, err
	}
	return groupRequestsListResult{Entries: resolveRequestEntries(ctx, requests, a.WA().ResolveLIDToPN)}, nil
}

func groupRequests(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	users, err := parseGroupUserJIDs(req.Users)
	if err != nil {
		return nil, err
	}
	updated, err := a.WA().UpdateGroupRequestParticipants(ctx, jid, users, wa.GroupParticipantRequestAction(req.SubAction))
	if err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupParticipantsResult{Updated: updated}, nil
}

func groupInvite(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	revoke := req.SubAction == "revoke"
	link, err := a.WA().GetGroupInviteLink(ctx, jid, revoke)
	if err != nil {
		return nil, err
	}
	return groupAckResult{JID: jid.String(), Link: link, Revoked: revoke}, nil
}

func groupJoin(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, err := a.WA().JoinGroupWithLink(ctx, strings.TrimSpace(req.Code))
	if err != nil {
		return nil, err
	}
	persistGroup(ctx, a, jid)
	return groupAckResult{JID: jid.String(), Joined: true}, nil
}

func groupLeave(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	if err := a.WA().LeaveGroup(ctx, jid); err != nil {
		return nil, err
	}
	_ = a.DB().MarkGroupLeft(jid.String(), time.Now().UTC())
	return groupAckResult{JID: jid.String(), Left: true}, nil
}

func groupInfo(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	jid, _ := parseGroupJID(req.GroupJID)
	info, err := a.WA().GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("group info not found for %s", jid.String())
	}
	_ = persistGroupInfo(ctx, a.DB(), a.WA(), info)
	return groupInfoResult{Info: info}, nil
}

func groupRefresh(ctx context.Context, a *app.App, req sendDelegateRequest) (any, error) {
	if err := ensureLive(ctx, a); err != nil {
		return nil, err
	}
	gs, err := a.WA().GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	joined := map[string]bool{}
	now := time.Now().UTC()
	for _, g := range gs {
		if g == nil {
			continue
		}
		joined[g.JID.String()] = true
		_ = persistGroupInfo(ctx, a.DB(), a.WA(), g)
		_ = a.DB().UpsertChatMetadata(g.JID.String(), "group", g.GroupName.Name)
	}
	if err := a.DB().MarkGroupsMissingFrom(joined, now); err != nil {
		return nil, err
	}
	return groupRefreshResult{Groups: len(gs)}, nil
}
