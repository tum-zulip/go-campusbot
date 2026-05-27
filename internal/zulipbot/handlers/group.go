package handlers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// AnnouncementUpdater triggers announcement re-render.
type AnnouncementUpdater interface {
	UpdateAfterMappingChange(ctx context.Context, send *announcement.SendParams) error
}

// GroupConfigReader provides announcement channel/topic config.
type GroupConfigReader interface {
	AnnouncementChannelID(ctx context.Context) (int64, bool, error)
	AnnouncementTopic(ctx context.Context) (string, bool, error)
}

// GroupHandler handles the "group" command.
type GroupHandler struct {
	client       channelgroup.Client
	repo         *storage.Repository
	announcer    AnnouncementUpdater
	configReader GroupConfigReader
	auth         command.Authorizer
}

// NewGroupHandler creates a new GroupHandler. It uses the channelgroup.Client
// directly for all Zulip and channel-group operations and the storage
// Repository for emoji-mapping and announcement-state persistence.
func NewGroupHandler(
	client channelgroup.Client,
	repo *storage.Repository,
	announcer AnnouncementUpdater,
	configReader GroupConfigReader,
	auth command.Authorizer,
) *GroupHandler {
	return &GroupHandler{
		client:       client,
		repo:         repo,
		announcer:    announcer,
		configReader: configReader,
		auth:         auth,
	}
}

func (h *GroupHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:    "group",
		Summary: "Subscribe or unsubscribe from a channel group.",
		Usage:   "group <subscribe|unsubscribe> [-k] <course_short_name>",
		AdminUsage: "group subscribe <course_short_name>\n" +
			"group unsubscribe [-k] <course_short_name>\n" +
			"group create <short_name> <emoji_name>\n" +
			"group available                       (user groups visible in Zulip — use the IDs with `group mapping set`)\n" +
			"group mapping <list|set <short_name> <zulip_user_group_id> <emoji_name>|disable <short_name>>\n" +
			"group channel <add|remove|create> <channel_id_or_name> <short_name>\n" +
			"group announce [set-message <message_id>|inspect]",
		Permission: command.PermOpen,
		ArgSpec:    GroupArgSpec,
	}
}

func (h *GroupHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	switch args := req.ParsedArgs.(type) {
	case GroupSubscribeArgs:
		return h.handleSubscribe(ctx, req, args)
	case GroupUnsubscribeArgs:
		return h.handleUnsubscribe(ctx, req, args)
	case GroupCreateArgs:
		return h.handleCreate(ctx, req, args)
	case GroupAvailableArgs:
		return h.handleAvailable(ctx, req)
	case GroupMappingListArgs:
		return h.handleMappingList(ctx, req)
	case GroupMappingSetArgs:
		return h.handleMappingSet(ctx, req, args)
	case GroupMappingDisableArgs:
		return h.handleMappingDisable(ctx, req, args)
	case GroupChannelAddArgs:
		return h.handleChannelAdd(ctx, req, args)
	case GroupChannelRemoveArgs:
		return h.handleChannelRemove(ctx, req, args)
	case GroupChannelCreateArgs:
		return h.handleChannelCreate(ctx, req, args)
	case GroupAnnounceArgs:
		return h.runAnnounce(ctx, req)
	case GroupAnnounceSetMessageArgs:
		return h.handleAnnounceSetMessage(ctx, req, args)
	case GroupAnnounceInspectArgs:
		return h.handleAnnounceInspect(ctx, req)
	default:
		return command.Result{}, command.NewUserError("Usage: `group <subscribe|unsubscribe> <course_short_name>`")
	}
}

//nolint:funlen // Keeps the create flow in one transaction-oriented handler.
func (h *GroupHandler) handleCreate(
	ctx context.Context,
	req command.Request,
	args GroupCreateArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	shortName := args.ShortName
	emojiName := args.EmojiName
	if strings.TrimSpace(shortName) == "" || strings.TrimSpace(emojiName) == "" {
		return command.Result{}, command.NewUserError("Usage: `group create <short_name> <emoji_name>`")
	}
	if err := h.ensureMappingDoesNotExist(ctx, shortName, emojiName); err != nil {
		return command.Result{}, err
	}

	channelGroupID, err := h.createChannelGroup(ctx, shortName, true)
	if err != nil {
		if isDuplicateZulipUserGroupError(err) {
			return command.Result{}, command.NewUserError(
				fmt.Sprintf(
					"Zulip user group `%s` already exists. Run `group available` to find its ID, then use that ID with `group mapping set`.",
					shortName,
				),
			)
		}
		return command.Result{}, fmt.Errorf("create channel group: %w", err)
	}
	createdMapping := false
	rollback := func(cause error) error {
		var rollbackErrs []error
		if createdMapping {
			if err := h.repo.DeleteEmojiGroupMappingByShortName(ctx, shortName); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if err := h.client.DeleteChannelGroup(ctx, channelGroupID); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if len(rollbackErrs) == 0 {
			return cause
		}
		return fmt.Errorf("%w (rollback failed: %w)", cause, errors.Join(rollbackErrs...))
	}

	now := time.Now().UTC()
	if err := h.repo.UpsertEmojiGroupMapping(ctx, storage.EmojiGroupMapping{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      0,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		return command.Result{}, rollback(fmt.Errorf("upsert emoji group mapping: %w", err))
	}
	createdMapping = true

	h.triggerAnnouncementUpdate(ctx)

	return command.Result{
		Content: fmt.Sprintf("Created channel group **%s** with Zulip user group ID %d and mapped `%s` → :%s:.",
			shortName, channelGroupID, shortName, emojiName),
	}, nil
}

// createChannelGroup creates a Zulip user group and tracks it locally as a
// channel group, subscribing the bot's own user as the initial member.
func (h *GroupHandler) createChannelGroup(ctx context.Context, name string, createChannelFolder bool) (int64, error) {
	ownUserResp, _, err := h.client.GetOwnUser(ctx).Execute()
	if err != nil {
		return 0, fmt.Errorf("get own Zulip user for channel group %q: %w", name, err)
	}
	if ownUserResp == nil || ownUserResp.User.UserID <= 0 {
		return 0, fmt.Errorf("get own Zulip user for channel group %q: missing user ID", name)
	}
	resp, _, err := h.client.CreateChannelGroup(ctx).
		CreateChannelFolder(createChannelFolder).
		Name(name).
		InitialSubscribers(zulip.UserIDsAsPrincipals(ownUserResp.User.UserID)).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("create channel group %q: %w", name, err)
	}
	return resp.ChannelGroupID, nil
}

func (h *GroupHandler) ensureMappingDoesNotExist(ctx context.Context, shortName, emojiName string) error {
	mappings, err := h.repo.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		if mapping.ShortName == shortName {
			return command.NewUserError(fmt.Sprintf("Mapping `%s` already exists.", shortName))
		}
		if mapping.Enabled && mapping.EmojiName == emojiName && mapping.ReactionType == "unicode_emoji" {
			return command.NewUserError(
				fmt.Sprintf("Emoji :%s: is already mapped to `%s`.", emojiName, mapping.ShortName),
			)
		}
	}
	return nil
}

func isDuplicateZulipUserGroupError(err error) bool {
	var coded zulip.CodedError
	if errors.As(err, &coded) && coded.Code == "BAD_REQUEST" &&
		strings.Contains(coded.Msg, "User group") &&
		strings.Contains(coded.Msg, "already exists") {
		return true
	}

	message := err.Error()
	return strings.Contains(message, "User group") &&
		strings.Contains(message, "already exists") &&
		strings.Contains(message, "BAD_REQUEST")
}

func (h *GroupHandler) handleSubscribe(
	ctx context.Context,
	req command.Request,
	args GroupSubscribeArgs,
) (command.Result, error) {
	shortName := args.ShortName

	mapping, found, err := h.repo.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(unknownGroupMessage(ctx, h.auth, req, shortName))
	}

	if _, _, err := h.client.SubscribeToChannelGroup(ctx, mapping.ChannelGroupID).
		Principals(zulip.Principals{UserIDs: &[]int64{req.Actor.UserID}}).
		Execute(); err != nil {
		return command.Result{}, fmt.Errorf("subscribe user to group: %w", err)
	}

	return command.Result{
		Content: fmt.Sprintf("You have been subscribed to **%s**.", mapping.ShortName),
	}, nil
}

func (h *GroupHandler) handleUnsubscribe(
	ctx context.Context,
	req command.Request,
	args GroupUnsubscribeArgs,
) (command.Result, error) {
	keepChannels := args.KeepChannels
	shortName := args.ShortName

	mapping, found, err := h.repo.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(unknownGroupMessage(ctx, h.auth, req, shortName))
	}

	req2 := h.client.UnsubscribeFromChannelGroup(ctx, mapping.ChannelGroupID).
		Principals(zulip.Principals{UserIDs: &[]int64{req.Actor.UserID}})
	if keepChannels {
		req2 = req2.KeepChannels()
	}
	if _, _, err := req2.Execute(); err != nil {
		if keepChannels {
			return command.Result{}, fmt.Errorf("unsubscribe user from group (keep channels): %w", err)
		}
		return command.Result{}, fmt.Errorf("unsubscribe user from group: %w", err)
	}

	if keepChannels {
		return command.Result{
			Content: fmt.Sprintf("You have been unsubscribed from **%s** (channels kept).", mapping.ShortName),
		}, nil
	}
	return command.Result{
		Content: fmt.Sprintf("You have been unsubscribed from **%s**.", mapping.ShortName),
	}, nil
}

// listVisibleZulipUserGroups returns the user groups visible to the bot account
// in Zulip, excluding deactivated and system groups. Sorted by ID.
func (h *GroupHandler) listVisibleZulipUserGroups(ctx context.Context) ([]channelgroup.ZulipUserGroupSummary, error) {
	resp, _, err := h.client.GetUserGroups(ctx).IncludeDeactivatedGroups(false).Execute()
	if err != nil {
		return nil, fmt.Errorf("list zulip user groups: %w", err)
	}
	summaries := make([]channelgroup.ZulipUserGroupSummary, 0, len(resp.UserGroups))
	for _, group := range resp.UserGroups {
		if group.Deactivated || group.IsSystemGroup {
			continue
		}
		summaries = append(summaries, channelgroup.ZulipUserGroupSummary{
			ID:            group.ID,
			Name:          group.Name,
			Description:   group.Description,
			MemberCount:   len(group.Members),
			IsSystemGroup: group.IsSystemGroup,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
	return summaries, nil
}

// channelGroupExists reports whether the channel group with the given ID exists
// in the local channelgroup database. Returns (false, nil) when missing.
func (h *GroupHandler) channelGroupExists(ctx context.Context, channelGroupID int64) (bool, error) {
	_, _, err := h.client.GetChannelGroup(ctx, channelGroupID).Execute()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, channelgroup.ErrChannelGroupNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("check channel group %d exists: %w", channelGroupID, err)
}

// handleAvailable lists user groups visible in Zulip to the bot account.
func (h *GroupHandler) handleAvailable(ctx context.Context, req command.Request) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}

	groups, err := h.listVisibleZulipUserGroups(ctx)
	if err != nil {
		return command.Result{}, err
	}
	if len(groups) == 0 {
		return command.Result{Content: "No Zulip channel groups/user groups visible to this bot."}, nil
	}

	var b strings.Builder
	b.WriteString("Zulip-visible user groups (use the id with `group mapping set`):\n")
	for _, g := range groups {
		name := g.Name
		if name == "" {
			name = "(no name)"
		}
		line := fmt.Sprintf("- id=%d **%s** (%d members)", g.ID, name, g.MemberCount)
		if g.Description != "" {
			line += fmt.Sprintf(" — %s", g.Description)
		}
		if g.IsSystemGroup {
			line += " _(system group)_"
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return command.Result{Content: strings.TrimSpace(b.String())}, nil
}

// unknownGroupMessage returns a permission-safe error message for an unknown channel group.
func unknownGroupMessage(ctx context.Context, auth command.Authorizer, req command.Request, shortName string) string {
	if auth.Check(ctx, req.Actor, command.PermAdmin) == nil {
		return fmt.Sprintf("Unknown channel group %q. Use `group mapping list` to see available groups.", shortName)
	}
	return fmt.Sprintf(
		"Unknown channel group %q. Use `help group` to see the command format, or ask an admin to check available groups.",
		shortName,
	)
}

func (h *GroupHandler) handleMappingList(ctx context.Context, req command.Request) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	mappings, err := h.repo.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		return command.Result{}, err
	}
	if len(mappings) == 0 {
		return command.Result{Content: "No emoji→group mappings configured."}, nil
	}

	var b strings.Builder
	b.WriteString("Emoji→group mappings:\n")
	for _, m := range mappings {
		status := "enabled"
		if !m.Enabled {
			status = "disabled"
		}
		annotation := ""
		exists, checkErr := h.channelGroupExists(ctx, m.ChannelGroupID)
		if checkErr != nil {
			annotation = " [check failed]"
		} else if !exists {
			annotation = " [missing channel group]"
		}
		b.WriteString(fmt.Sprintf("- `%s`: :%s: → group %d [%s]%s\n",
			m.ShortName, m.EmojiName, m.ChannelGroupID, status, annotation))
	}
	return command.Result{Content: strings.TrimSpace(b.String())}, nil
}

// validateEnabledMappings returns the list of enabled mappings that reference a
// missing channel group.
func (h *GroupHandler) validateEnabledMappings(ctx context.Context) ([]storage.EmojiGroupMapping, error) {
	mappings, err := h.repo.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return nil, err
	}
	var invalid []storage.EmojiGroupMapping
	for _, m := range mappings {
		exists, err := h.channelGroupExists(ctx, m.ChannelGroupID)
		if err != nil {
			return nil, fmt.Errorf("verify channel group %d exists: %w", m.ChannelGroupID, err)
		}
		if !exists {
			invalid = append(invalid, m)
		}
	}
	return invalid, nil
}

func (h *GroupHandler) handleMappingSet(
	ctx context.Context,
	req command.Request,
	args GroupMappingSetArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	shortName := args.ShortName
	channelGroupID := args.ZulipGroupID
	emojiName := args.EmojiName

	if channelGroupID <= 0 {
		return command.Result{}, command.NewUserError("zulip_user_group_id must be a positive integer")
	}

	imported, err := h.ensureChannelGroupImported(ctx, channelGroupID)
	if err != nil {
		return command.Result{}, err
	}

	mapping := storage.EmojiGroupMapping{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      0,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := h.repo.UpsertEmojiGroupMapping(ctx, mapping); err != nil {
		return command.Result{}, fmt.Errorf("upsert emoji group mapping: %w", err)
	}

	h.triggerAnnouncementUpdate(ctx)

	if imported {
		return command.Result{
			Content: fmt.Sprintf("Imported Zulip group %d and mapped `%s` → :%s:.",
				channelGroupID, shortName, emojiName),
		}, nil
	}
	return command.Result{
		Content: fmt.Sprintf("Mapped `%s` → :%s: (group %d).",
			shortName, emojiName, channelGroupID),
	}, nil
}

// zulipGroupVisible reports whether the bot can see a user group with the given
// ID in Zulip.
func (h *GroupHandler) zulipGroupVisible(ctx context.Context, userGroupID int64) (bool, error) {
	groups, err := h.listVisibleZulipUserGroups(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g.ID == userGroupID {
			return true, nil
		}
	}
	return false, nil
}

func (h *GroupHandler) ensureChannelGroupImported(ctx context.Context, channelGroupID int64) (bool, error) {
	exists, err := h.channelGroupExists(ctx, channelGroupID)
	if err != nil {
		return false, fmt.Errorf("verify channel group %d exists: %w", channelGroupID, err)
	}
	if exists {
		return false, nil
	}
	visible, err := h.zulipGroupVisible(ctx, channelGroupID)
	if err != nil {
		return false, fmt.Errorf("check zulip visibility for group %d: %w", channelGroupID, err)
	}
	if !visible {
		return false, command.NewUserError(fmt.Sprintf(
			"Channel group %d is not visible in Zulip. Run `group available` to see available groups.",
			channelGroupID,
		))
	}
	if err := h.client.ImportZulipUserGroup(ctx, channelGroupID); err != nil {
		return false, fmt.Errorf("auto-import channel group %d: %w", channelGroupID, err)
	}
	return true, nil
}

func (h *GroupHandler) handleMappingDisable(
	ctx context.Context,
	req command.Request,
	args GroupMappingDisableArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	shortName := args.ShortName

	if err := h.repo.SetEmojiGroupMappingEnabled(ctx, shortName, false); err != nil {
		return command.Result{}, fmt.Errorf("disable emoji group mapping: %w", err)
	}

	h.triggerAnnouncementUpdate(ctx)

	return command.Result{
		Content: fmt.Sprintf("Mapping `%s` disabled.", shortName),
	}, nil
}

func (h *GroupHandler) runAnnounce(ctx context.Context, req command.Request) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	invalid, err := h.validateEnabledMappings(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("validate emoji mappings: %w", err)
	}
	if len(invalid) > 0 {
		var refs []string
		for _, m := range invalid {
			refs = append(refs, fmt.Sprintf("%s -> channel_group_id=%d", m.ShortName, m.ChannelGroupID))
		}
		return command.Result{}, command.NewUserError(fmt.Sprintf(
			"Cannot update announcement: enabled mapping(s) reference missing channel group(s): %s. "+
				"Disable or fix the mapping, or create/import the channel group first.",
			strings.Join(refs, "; "),
		))
	}

	state, ok, err := h.repo.GetAnnouncementState(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("read announcement state: %w", err)
	}

	var send *announcement.SendParams
	if !ok || state.MessageID == nil {
		channelID, channelOK, err := h.configReader.AnnouncementChannelID(ctx)
		if err != nil {
			return command.Result{}, fmt.Errorf("read announcement channel ID: %w", err)
		}
		topic, topicOK, err := h.configReader.AnnouncementTopic(ctx)
		if err != nil {
			return command.Result{}, fmt.Errorf("read announcement topic: %w", err)
		}
		if !channelOK {
			return command.Result{}, command.NewUserError(
				"No announcement message configured. Either:\n" +
					"- Run `group announce set-message <id>` to use an existing message, or\n" +
					"- Set `announcement.channel_id` and `announcement.topic` to create a new message.",
			)
		}
		if !topicOK {
			return command.Result{}, command.NewUserError(
				"Announcement topic not configured. Set `announcement.topic` first.",
			)
		}
		send = &announcement.SendParams{ChannelID: channelID, Topic: topic}
	}

	if err := h.announcer.UpdateAfterMappingChange(ctx, send); err != nil {
		return command.Result{}, fmt.Errorf("send/update announcement: %w", err)
	}

	return command.Result{Content: "Announcement updated."}, nil
}

func (h *GroupHandler) handleAnnounceSetMessage(
	ctx context.Context,
	req command.Request,
	args GroupAnnounceSetMessageArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	msgID := args.MessageID
	if msgID <= 0 {
		return command.Result{}, command.NewUserError("message_id must be a positive integer")
	}

	if err := h.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "",
	}); err != nil {
		return command.Result{}, fmt.Errorf("save announcement state: %w", err)
	}

	return command.Result{
		Content: fmt.Sprintf(
			"Announcement message ID set to %d. Run `group announce` to update the message content and add reactions.",
			msgID,
		),
	}, nil
}

func (h *GroupHandler) handleAnnounceInspect(ctx context.Context, req command.Request) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	state, ok, err := h.repo.GetAnnouncementState(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("read announcement state: %w", err)
	}

	channelID, channelOK, _ := h.configReader.AnnouncementChannelID(ctx)
	topic, topicOK, _ := h.configReader.AnnouncementTopic(ctx)

	var b strings.Builder
	b.WriteString("**Announcement configuration:**\n")

	if ok && state.MessageID != nil {
		b.WriteString(fmt.Sprintf("- message_id: %d ✓\n", *state.MessageID))
		b.WriteString("- mode: **edit existing message** (channel_id and topic not required)\n")
		if !state.UpdatedAt.IsZero() {
			b.WriteString(fmt.Sprintf("- last updated: %s\n", state.UpdatedAt.Format(time.RFC3339)))
		}
	} else {
		b.WriteString("- message_id: not set\n")
		b.WriteString("- mode: **create new message** (channel_id and topic required)\n")
	}

	if channelOK {
		b.WriteString(fmt.Sprintf("- channel_id: %d\n", channelID))
	} else {
		b.WriteString("- channel_id: not configured\n")
	}

	if topicOK {
		b.WriteString(fmt.Sprintf("- topic: %s\n", topic))
	} else {
		b.WriteString("- topic: not configured\n")
	}

	return command.Result{Content: strings.TrimSpace(b.String())}, nil
}

func (h *GroupHandler) handleChannelModify(
	ctx context.Context,
	channelID int64,
	shortName string,
	op func(ctx context.Context, groupID, channelID int64) error,
	successFmt string,
) (command.Result, error) {
	if channelID <= 0 {
		return command.Result{}, command.NewUserError("channel_id must be a positive integer")
	}

	mapping, found, err := h.repo.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	if err := op(ctx, mapping.ChannelGroupID, channelID); err != nil {
		return command.Result{}, fmt.Errorf("channel group operation: %w", err)
	}
	return command.Result{Content: fmt.Sprintf(successFmt, channelID, mapping.ShortName)}, nil
}

func (h *GroupHandler) addChannelToGroup(ctx context.Context, channelGroupID, channelID int64) error {
	_, _, err := h.client.UpdateChannelGroupChannels(ctx, channelGroupID).
		Add([]int64{channelID}).
		Execute()
	if err != nil {
		return fmt.Errorf("add channel %d to channel group %d: %w", channelID, channelGroupID, err)
	}
	return nil
}

func (h *GroupHandler) removeChannelFromGroup(ctx context.Context, channelGroupID, channelID int64) error {
	_, _, err := h.client.UpdateChannelGroupChannels(ctx, channelGroupID).
		Delete([]int64{channelID}).
		Execute()
	if err != nil {
		return fmt.Errorf("remove channel %d from channel group %d: %w", channelID, channelGroupID, err)
	}
	return nil
}

func (h *GroupHandler) handleChannelAdd(
	ctx context.Context,
	req command.Request,
	args GroupChannelAddArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	return h.handleChannelModify(ctx, args.ChannelID, args.ShortName,
		h.addChannelToGroup, "Added channel %d to **%s**.")
}

func (h *GroupHandler) handleChannelRemove(
	ctx context.Context,
	req command.Request,
	args GroupChannelRemoveArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	return h.handleChannelModify(ctx, args.ChannelID, args.ShortName,
		h.removeChannelFromGroup, "Removed channel %d from **%s**.")
}

func (h *GroupHandler) handleChannelCreate(
	ctx context.Context,
	req command.Request,
	args GroupChannelCreateArgs,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	channelName := args.ChannelName
	shortName := args.ShortName

	mapping, found, err := h.repo.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	channelID, err := h.createChannelAndAddToGroup(ctx, channelName, mapping.ChannelGroupID)
	if err != nil {
		return command.Result{}, fmt.Errorf("create channel and add to group: %w", err)
	}
	return command.Result{
		Content: fmt.Sprintf(
			"Created channel **%s** (id=%d) and added it to **%s**.",
			channelName,
			channelID,
			mapping.ShortName,
		),
	}, nil
}

// createChannelAndAddToGroup creates a new Zulip channel and adds it to the
// specified group. The bot account is subscribed as the initial member.
func (h *GroupHandler) createChannelAndAddToGroup(
	ctx context.Context,
	channelName string,
	channelGroupID int64,
) (int64, error) {
	ownUserResp, _, err := h.client.GetOwnUser(ctx).Execute()
	if err != nil {
		return 0, fmt.Errorf("get own Zulip user for channel creation: %w", err)
	}
	if ownUserResp == nil || ownUserResp.User.UserID <= 0 {
		return 0, errors.New("get own Zulip user for channel creation: missing user ID")
	}
	channelResp, _, err := h.client.CreateChannel(ctx).
		Name(channelName).
		Subscribers([]int64{ownUserResp.User.UserID}).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("create channel %q: %w", channelName, err)
	}
	if err := h.addChannelToGroup(ctx, channelGroupID, channelResp.ID); err != nil {
		return 0, fmt.Errorf("add channel %d to group %d: %w", channelResp.ID, channelGroupID, err)
	}
	return channelResp.ID, nil
}

func (h *GroupHandler) triggerAnnouncementUpdate(ctx context.Context) {
	state, ok, err := h.repo.GetAnnouncementState(ctx)
	if err != nil {
		return
	}

	var send *announcement.SendParams
	if !ok || state.MessageID == nil {
		channelID, channelOK, err := h.configReader.AnnouncementChannelID(ctx)
		if err != nil || !channelOK {
			return
		}
		topic, topicOK, err := h.configReader.AnnouncementTopic(ctx)
		if err != nil || !topicOK {
			return
		}
		send = &announcement.SendParams{ChannelID: channelID, Topic: topic}
	}

	if err := h.announcer.UpdateAfterMappingChange(ctx, send); err != nil {
		_ = err
	}
}

var _ command.Handler = (*GroupHandler)(nil)

var _ zulip.Role = command.PermAdmin
