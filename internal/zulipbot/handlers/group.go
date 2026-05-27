package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

// Config keys for the channel-group announcement message. Kept here (rather
// than in the parent zulipbot package) so the handler can read them directly
// from storage without an interface indirection; zulipbot/config.go imports
// these to register them as user-facing config values.
const (
	KeyAnnouncementChannelID = "announcement.channel_id"
	KeyAnnouncementTopic     = "announcement.topic"
)

// announceTarget carries the channel/topic used when no announcement message
// has been sent yet. Required only on the initial send.
type announceTarget struct {
	channelID int64
	topic     string
}

// GroupHandler handles the "group" command.
type GroupHandler struct {
	client  channelgroup.Client
	queries *storagedb.Queries
	auth    command.Authorizer
	logger  *slog.Logger
}

// NewGroupHandler creates a new GroupHandler. It uses the channelgroup.Client
// directly for all Zulip and channel-group operations and the storage
// Repository for emoji-mapping, announcement-state, and config persistence.
func NewGroupHandler(
	client channelgroup.Client,
	queries *storagedb.Queries,
	auth command.Authorizer,
	logger *slog.Logger,
) *GroupHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &GroupHandler{
		client:  client,
		queries: queries,
		auth:    auth,
		logger:  logger,
	}
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (h *GroupHandler) emojiGroupMappingByShortName(
	ctx context.Context,
	shortName string,
) (storagedb.EmojiGroupMapping, bool, error) {
	row, err := h.queries.GetEmojiGroupMappingByShortName(ctx, shortName)
	if errors.Is(err, sql.ErrNoRows) {
		return storagedb.EmojiGroupMapping{}, false, nil
	}
	if err != nil {
		return storagedb.EmojiGroupMapping{}, false, fmt.Errorf(
			"get emoji group mapping by short name %q: %w",
			shortName,
			err,
		)
	}
	return row, true, nil
}

func (h *GroupHandler) announcementState(ctx context.Context) (storagedb.AnnouncementState, bool, error) {
	row, err := h.queries.GetAnnouncementState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return storagedb.AnnouncementState{}, false, nil
	}
	if err != nil {
		return storagedb.AnnouncementState{}, false, fmt.Errorf("get announcement state: %w", err)
	}
	return row, true, nil
}

func (h *GroupHandler) saveAnnouncementState(
	ctx context.Context,
	messageID sql.NullInt64,
	contentHash string,
) error {
	if err := h.queries.SaveAnnouncementState(ctx, storagedb.SaveAnnouncementStateParams{
		MessageID:   messageID,
		ContentHash: contentHash,
		UpdatedAt:   formatTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("save announcement state: %w", err)
	}
	return nil
}

// announcementChannelID returns the configured announcement channel ID. Returns
// (0, false, nil) when unset or empty.
func (h *GroupHandler) announcementChannelID(ctx context.Context) (int64, bool, error) {
	raw, err := h.queries.GetConfigValue(ctx, KeyAnnouncementChannelID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if raw == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s=%q: %w", KeyAnnouncementChannelID, raw, err)
	}
	return id, true, nil
}

// announcementTopic returns the configured announcement topic. Returns
// ("", false, nil) when unset or empty.
func (h *GroupHandler) announcementTopic(ctx context.Context) (string, bool, error) {
	raw, err := h.queries.GetConfigValue(ctx, KeyAnnouncementTopic)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if raw == "" {
		return "", false, nil
	}
	return raw, true, nil
}

func (h *GroupHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:    "group",
		Summary: "Subscribe or unsubscribe from a channel group.",
		Usage:   "group <subscribe|unsubscribe> [-k] <course_short_name>",
		AdminUsage: "group subscribe <course_short_name>\n" +
			"group unsubscribe [-k] <course_short_name>\n" +
			"group create <short_name> <emoji_name>\n" +
			"group mapping <list|set <short_name> <zulip_user_group> <emoji_name>|disable <short_name>>\n" +
			"group channel <add|remove|create> <channel_mention_or_name> <short_name>\n" +
			"group folder <add|remove|assign|unassign> <short_name>\n" +
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
	case GroupFolderAddArgs:
		return h.handleFolderModify(ctx, req, args.ShortName, h.addFolderToGroup, "Added channel folder to **%s**.")
	case GroupFolderRemoveArgs:
		return h.handleFolderModify(ctx, req, args.ShortName, h.removeFolderFromGroup, "Removed channel folder from **%s**.")
	case GroupFolderAssignArgs:
		return h.handleFolderModify(ctx, req, args.ShortName, h.assignFolderToGroup, "Assigned channel folder for **%s**.")
	case GroupFolderUnassignArgs:
		return h.handleFolderModify(ctx, req, args.ShortName, h.unassignFolderFromGroup, "Unassigned channel folder for **%s**.")
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
					"Zulip user group `%s` already exists. Mention the existing user group with `group mapping set`.",
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
			if err := h.queries.DeleteEmojiGroupMappingByShortName(ctx, shortName); err != nil {
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

	now := formatTime(time.Now())
	if err := h.queries.UpsertEmojiGroupMapping(ctx, storagedb.UpsertEmojiGroupMappingParams{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        1,
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
	mappings, err := h.queries.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		return err
	}
	for _, mapping := range mappings {
		if mapping.ShortName == shortName {
			return command.NewUserError(fmt.Sprintf("Mapping `%s` already exists.", shortName))
		}
		if mapping.Enabled != 0 && mapping.EmojiName == emojiName && mapping.ReactionType == "unicode_emoji" {
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

	mapping, found, err := h.emojiGroupMappingByShortName(ctx, shortName)
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

	mapping, found, err := h.emojiGroupMappingByShortName(ctx, shortName)
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
	mappings, err := h.queries.ListAllEmojiGroupMappings(ctx)
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
		if m.Enabled == 0 {
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
func (h *GroupHandler) validateEnabledMappings(ctx context.Context) ([]storagedb.EmojiGroupMapping, error) {
	mappings, err := h.queries.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return nil, err
	}
	var invalid []storagedb.EmojiGroupMapping
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
	channelGroupID := args.ZulipGroup.UserID
	emojiName := args.EmojiName

	if channelGroupID <= 0 {
		return command.Result{}, command.NewUserError("zulip_user_group must resolve to a valid Zulip user group")
	}
	if err := h.ensureZulipUserIsVisibleUserGroup(ctx, args.ZulipGroup); err != nil {
		return command.Result{}, err
	}

	imported, err := h.ensureChannelGroupImported(ctx, channelGroupID)
	if err != nil {
		return command.Result{}, err
	}

	now := formatTime(time.Now())
	mapping := storagedb.UpsertEmojiGroupMappingParams{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        1,
		SortOrder:      0,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := h.queries.UpsertEmojiGroupMapping(ctx, mapping); err != nil {
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

func (h *GroupHandler) ensureZulipUserIsVisibleUserGroup(ctx context.Context, user zulip.User) error {
	groups, err := h.listVisibleZulipUserGroups(ctx)
	if err != nil {
		return fmt.Errorf("verify Zulip user group %d: %w", user.UserID, err)
	}
	for _, group := range groups {
		if group.ID == user.UserID {
			return nil
		}
	}
	name := user.FullName
	if name == "" {
		name = fmt.Sprintf("id=%d", user.UserID)
	}
	return command.NewUserError(fmt.Sprintf(
		"%s is not a visible Zulip user group. Mention a Zulip user group visible to the bot.",
		name,
	))
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
			"Channel group %d is not visible in Zulip. Mention a Zulip user group visible to the bot.",
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

	if err := h.queries.SetEmojiGroupMappingEnabled(ctx, storagedb.SetEmojiGroupMappingEnabledParams{
		Enabled:   0,
		UpdatedAt: formatTime(time.Now()),
		ShortName: shortName,
	}); err != nil {
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

	state, ok, err := h.announcementState(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("read announcement state: %w", err)
	}

	var target *announceTarget
	if !ok || !state.MessageID.Valid {
		channelID, channelOK, err := h.announcementChannelID(ctx)
		if err != nil {
			return command.Result{}, fmt.Errorf("read announcement channel ID: %w", err)
		}
		topic, topicOK, err := h.announcementTopic(ctx)
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
		target = &announceTarget{channelID: channelID, topic: topic}
	}

	if err := h.ensureAnnouncement(ctx, target); err != nil {
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

	if err := h.saveAnnouncementState(ctx, sql.NullInt64{Int64: msgID, Valid: true}, ""); err != nil {
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
	state, ok, err := h.announcementState(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("read announcement state: %w", err)
	}

	channelID, channelOK, _ := h.announcementChannelID(ctx)
	topic, topicOK, _ := h.announcementTopic(ctx)

	var b strings.Builder
	b.WriteString("**Announcement configuration:**\n")

	if ok && state.MessageID.Valid {
		b.WriteString(fmt.Sprintf("- message_id: %d ✓\n", state.MessageID.Int64))
		b.WriteString("- mode: **edit existing message** (channel_id and topic not required)\n")
		if state.UpdatedAt != "" {
			b.WriteString(fmt.Sprintf("- last updated: %s\n", state.UpdatedAt))
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

	mapping, found, err := h.emojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	if err := op(ctx, mapping.ChannelGroupID, channelID); err != nil {
		if userErr, ok := folderUserError(err, mapping.ShortName); ok {
			return command.Result{}, userErr
		}
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

func (h *GroupHandler) handleFolderModify(
	ctx context.Context,
	req command.Request,
	shortName string,
	op func(ctx context.Context, groupID int64) error,
	successFmt string,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}

	mapping, found, err := h.emojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	if err := op(ctx, mapping.ChannelGroupID); err != nil {
		if userErr, ok := folderUserError(err, mapping.ShortName); ok {
			return command.Result{}, userErr
		}
		return command.Result{}, fmt.Errorf("channel group folder operation: %w", err)
	}
	return command.Result{Content: fmt.Sprintf(successFmt, mapping.ShortName)}, nil
}

func folderUserError(err error, shortName string) (command.UserError, bool) {
	var folderConflict channelgroup.ChannelFolderConflictError
	if errors.As(err, &folderConflict) {
		return command.NewUserError(fmt.Sprintf(
			"Channel %d is in another channel folder. Remove it from that folder before changing the folder for **%s**.",
			folderConflict.ChannelID,
			shortName,
		)), true
	}

	var externalChannel channelgroup.ChannelFolderExternalChannelError
	if errors.As(err, &externalChannel) {
		if externalChannel.ChannelID == 0 {
			return command.NewUserError(fmt.Sprintf(
				"This channel folder still contains channels outside **%s**. Remove those channels from the folder before removing the group folder.",
				shortName,
			)), true
		}
		return command.NewUserError(fmt.Sprintf(
			"Channel %d is in this channel folder but is not part of **%s**. Remove it from the folder before removing the group folder.",
			externalChannel.ChannelID,
			shortName,
		)), true
	}
	return command.UserError{}, false
}

func (h *GroupHandler) addFolderToGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := h.client.UpdateChannelGroupFolder(ctx, channelGroupID).Add().Execute()
	if err != nil {
		return fmt.Errorf("add channel folder to channel group %d: %w", channelGroupID, err)
	}
	return nil
}

func (h *GroupHandler) removeFolderFromGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := h.client.UpdateChannelGroupFolder(ctx, channelGroupID).Remove().Execute()
	if err != nil {
		return fmt.Errorf("remove channel folder from channel group %d: %w", channelGroupID, err)
	}
	return nil
}

func (h *GroupHandler) assignFolderToGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := h.client.UpdateChannelGroupFolder(ctx, channelGroupID).Assign().Execute()
	if err != nil {
		return fmt.Errorf("assign channel folder for channel group %d: %w", channelGroupID, err)
	}
	return nil
}

func (h *GroupHandler) unassignFolderFromGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := h.client.UpdateChannelGroupFolder(ctx, channelGroupID).Unassign().Execute()
	if err != nil {
		return fmt.Errorf("unassign channel folder for channel group %d: %w", channelGroupID, err)
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
	return h.handleChannelModify(ctx, args.Channel.ChannelID, args.ShortName,
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
	return h.handleChannelModify(ctx, args.Channel.ChannelID, args.ShortName,
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

	mapping, found, err := h.emojiGroupMappingByShortName(ctx, shortName)
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
	state, ok, err := h.announcementState(ctx)
	if err != nil {
		return
	}

	var target *announceTarget
	if !ok || !state.MessageID.Valid {
		channelID, channelOK, err := h.announcementChannelID(ctx)
		if err != nil || !channelOK {
			return
		}
		topic, topicOK, err := h.announcementTopic(ctx)
		if err != nil || !topicOK {
			return
		}
		target = &announceTarget{channelID: channelID, topic: topic}
	}

	if err := h.ensureAnnouncement(ctx, target); err != nil {
		h.logger.WarnContext(ctx, "announcement update failed", "error", err)
	}
}

// ensureAnnouncement sends or edits the channel-group announcement message.
//   - If no message_id is stored: send a new message to the supplied channel/topic
//     and persist the message_id. Requires a non-nil target with positive channelID
//     and non-empty topic.
//   - If a message_id is stored: edit the message when the rendered content has
//     changed; otherwise leave it alone.
//
// After a send or edit, the bot's reactions for every enabled mapping are added.
// Reaction errors are logged but never propagated.
//
//nolint:nestif // send-vs-edit branches share state and are clearer than extracting partial flows
func (h *GroupHandler) ensureAnnouncement(ctx context.Context, target *announceTarget) error {
	mappings, err := h.queries.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return fmt.Errorf("list emoji group mappings: %w", err)
	}

	content := RenderAnnouncement(mappings)
	hash := AnnouncementContentHash(mappings)

	state, ok, err := h.announcementState(ctx)
	if err != nil {
		return fmt.Errorf("get announcement state: %w", err)
	}

	var messageID int64
	if !ok || !state.MessageID.Valid {
		if target == nil || target.channelID <= 0 || target.topic == "" {
			return errors.New("no announcement message_id stored and no channel/topic provided: " +
				"run `group announce set-message <id>` to migrate from an existing message, " +
				"or set announcement.channel_id and announcement.topic to create a new one",
			)
		}
		resp, _, err := h.client.SendMessage(ctx).
			To(zulip.ChannelAsRecipient(target.channelID)).
			Topic(target.topic).
			Content(content).
			Execute()
		if err != nil {
			return fmt.Errorf("send announcement message: %w", err)
		}
		if resp == nil {
			return errors.New("send announcement message: empty response")
		}
		messageID = resp.ID
		if err := h.saveAnnouncementState(ctx, sql.NullInt64{Int64: messageID, Valid: true}, hash); err != nil {
			return fmt.Errorf("save announcement state: %w", err)
		}
		h.logger.InfoContext(ctx, "sent new announcement message", "message_id", messageID)
	} else {
		messageID = state.MessageID.Int64
		if state.ContentHash != hash {
			if _, _, err := h.client.UpdateMessage(ctx, messageID).Content(content).Execute(); err != nil {
				return fmt.Errorf("edit announcement message %d: %w", messageID, err)
			}
			if err := h.saveAnnouncementState(ctx, sql.NullInt64{Int64: messageID, Valid: true}, hash); err != nil {
				return fmt.Errorf("save announcement state: %w", err)
			}
			h.logger.InfoContext(ctx, "updated announcement message", "message_id", messageID)
		} else {
			h.logger.InfoContext(ctx, "announcement message is up to date", "message_id", messageID)
		}
	}

	h.addAnnouncementReactions(ctx, messageID, mappings)
	return nil
}

func (h *GroupHandler) addAnnouncementReactions(
	ctx context.Context,
	messageID int64,
	mappings []storagedb.EmojiGroupMapping,
) {
	for _, mapping := range mappings {
		req := h.client.AddReaction(ctx, messageID).EmojiName(mapping.EmojiName)
		if mapping.EmojiCode != "" {
			req = req.EmojiCode(mapping.EmojiCode)
		}
		if mapping.ReactionType != "" {
			req = req.ReactionType(mapping.ReactionType)
		}
		if _, _, err := req.Execute(); err != nil {
			h.logger.WarnContext(ctx, "failed to add bot reaction to announcement",
				"message_id", messageID,
				"emoji_name", mapping.EmojiName,
				"error", err)
		}
	}
}

var _ command.Handler = (*GroupHandler)(nil)

var _ zulip.Role = command.PermAdmin
