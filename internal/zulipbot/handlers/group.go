package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// GroupSubscriber subscribes or unsubscribes a user from a channel group.
type GroupSubscriber interface {
	SubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error
	UnsubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error
	UnsubscribeUserKeepChannels(ctx context.Context, userID int64, channelGroupID int64) error
}

// ChannelGroupChecker reports on the channel groups known to the bot.
// ChannelGroupExists is used to reject — or, for emoji mappings, auto-import
// — references to a channel group before they can poison the announcement or
// reaction-handling flow. ListZulipUserGroups powers the admin `group
// available` command and queries Zulip live so admins can see which user
// groups the bot account can see. ImportZulipUserGroup is invoked from
// `group mapping set` to auto-track a Zulip user group locally on first use.
type ChannelGroupChecker interface {
	ChannelGroupExists(ctx context.Context, channelGroupID int64) (bool, error)
	ListZulipUserGroups(ctx context.Context) ([]channelgroup.ZulipUserGroupSummary, error)
	ImportZulipUserGroup(ctx context.Context, userGroupID int64) error
	CreateChannelGroup(ctx context.Context, name string, createChannelFolder bool) (int64, error)
	DeleteChannelGroup(ctx context.Context, channelGroupID int64) error
}

// GroupMappingReader looks up emoji->group mappings.
type GroupMappingReader interface {
	GetEmojiGroupMappingByShortName(ctx context.Context, shortName string) (storage.EmojiGroupMapping, bool, error)
	ListEnabledEmojiGroupMappings(ctx context.Context) ([]storage.EmojiGroupMapping, error)
	ListAllEmojiGroupMappings(ctx context.Context) ([]storage.EmojiGroupMapping, error)
}

// GroupMappingWriter writes emoji->group mappings.
type GroupMappingWriter interface {
	UpsertEmojiGroupMapping(ctx context.Context, m storage.EmojiGroupMapping) error
	SetEmojiGroupMappingEnabled(ctx context.Context, shortName string, enabled bool) error
	DeleteEmojiGroupMappingByShortName(ctx context.Context, shortName string) error
}

// ChannelGroupChannelManager adds, removes, and creates channels in a channel group.
type ChannelGroupChannelManager interface {
	AddChannelToGroup(ctx context.Context, channelGroupID int64, channelID int64) error
	RemoveChannelFromGroup(ctx context.Context, channelGroupID int64, channelID int64) error
	CreateChannelAndAddToGroup(ctx context.Context, channelName string, channelGroupID int64) (int64, error)
}

// AnnouncementUpdater triggers announcement re-render.
type AnnouncementUpdater interface {
	UpdateAfterMappingChange(ctx context.Context, send *announcement.SendParams) error
}

// AnnouncementStateAccessor reads and writes announcement state.
type AnnouncementStateAccessor interface {
	GetAnnouncementState(ctx context.Context) (storage.AnnouncementState, bool, error)
	SaveAnnouncementState(ctx context.Context, state storage.AnnouncementState) error
}

// GroupConfigReader provides announcement channel/topic config.
type GroupConfigReader interface {
	AnnouncementChannelID(ctx context.Context) (int64, bool, error)
	AnnouncementTopic(ctx context.Context) (string, bool, error)
}

// GroupHandler handles the "group" command.
type GroupHandler struct {
	subscriber     GroupSubscriber
	groupChecker   ChannelGroupChecker
	mappingReader  GroupMappingReader
	mappingWriter  GroupMappingWriter
	announcer      AnnouncementUpdater
	stateAccessor  AnnouncementStateAccessor
	configReader   GroupConfigReader
	auth           command.Authorizer
	channelManager ChannelGroupChannelManager
}

// NewGroupHandler creates a new GroupHandler.
func NewGroupHandler(
	subscriber GroupSubscriber,
	groupChecker ChannelGroupChecker,
	mappingReader GroupMappingReader,
	mappingWriter GroupMappingWriter,
	announcer AnnouncementUpdater,
	stateAccessor AnnouncementStateAccessor,
	configReader GroupConfigReader,
	auth command.Authorizer,
) *GroupHandler {
	return &GroupHandler{
		subscriber:    subscriber,
		groupChecker:  groupChecker,
		mappingReader: mappingReader,
		mappingWriter: mappingWriter,
		announcer:     announcer,
		stateAccessor: stateAccessor,
		configReader:  configReader,
		auth:          auth,
	}
}

// WithChannelManager sets the channel manager used by the course subcommand.
func (h *GroupHandler) WithChannelManager(m ChannelGroupChannelManager) *GroupHandler {
	h.channelManager = m
	return h
}

func (h *GroupHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:    "group",
		Summary: "Subscribe or unsubscribe from a channel group.",
		Usage:   "group <subscribe|unsubscribe> [-k] <course_short_name>",
		// AdminUsage lists all subcommands visible to admins and owners.
		// Normal users only see the public subscribe/unsubscribe form above.
		AdminUsage: "group subscribe <course_short_name>\n" +
			"group unsubscribe [-k] <course_short_name>\n" +
			"group create <short_name> <emoji_name>\n" +
			"group available                       (user groups visible in Zulip — use the IDs with `group mapping set`)\n" +
			"group mapping <list|set <short_name> <zulip_user_group_id> <emoji_name>|disable <short_name>>\n" +
			"group channel <add|remove|create> <channel_id_or_name> <short_name>\n" +
			"group announce [set-message <message_id>|inspect]",
		Permission: command.PermOpen,
	}
}

// Handle dispatches subcommands:
//
//	group subscribe <course_short_name>
//	group unsubscribe <course_short_name>
//	group unsubscribe -k <course_short_name>
//	group create <short_name> <emoji_name> (admin only)
//	group available                       (admin only — user groups visible in Zulip)
//	group mapping list                    (admin only)
//	group mapping set <short_name> <zulip_user_group_id> <emoji_name> (admin only — auto-imports the Zulip group if needed)
//	group mapping disable <name>          (admin only)
//	group channel add <channel_id> <short_name>    (admin only)
//	group channel remove <channel_id> <short_name> (admin only)
//	group channel create <channel_name> <short_name> (admin only)
//	group course ...                      (alias for group channel ...)
func (h *GroupHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	args := req.Invocation.Args
	if len(args) == 0 {
		return command.Result{}, command.NewUserError("Usage: `group <subscribe|unsubscribe> [-k] <course_short_name>`")
	}

	switch args[0] {
	case "subscribe":
		return h.handleSubscribe(ctx, req, args[1:])
	case "unsubscribe":
		return h.handleUnsubscribe(ctx, req, args[1:])
	case "create":
		return h.handleCreate(ctx, req, args[1:])
	case "available":
		return h.handleAvailable(ctx, req)
	case "mapping":
		return h.handleMapping(ctx, req, args[1:])
	case "channel", "course":
		return h.handleChannel(ctx, req, args[1:])
	case "announce":
		return h.handleAnnounce(ctx, req, args[1:])
	default:
		return command.Result{}, command.NewUserError(
			fmt.Sprintf(
				"Unknown subcommand %q. Usage: `group <subscribe|unsubscribe> [-k] <course_short_name>`",
				args[0],
			),
		)
	}
}

//nolint:funlen // Keeps the create flow in one transaction-oriented handler.
func (h *GroupHandler) handleCreate(
	ctx context.Context,
	req command.Request,
	args []string,
) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	if len(args) != 2 { //nolint:mnd // Command requires short name and emoji name.
		return command.Result{}, command.NewUserError("Usage: `group create <short_name> <emoji_name>`")
	}
	shortName := args[0]
	emojiName := args[1]
	if strings.TrimSpace(shortName) == "" || strings.TrimSpace(emojiName) == "" {
		return command.Result{}, command.NewUserError("Usage: `group create <short_name> <emoji_name>`")
	}
	if err := h.ensureMappingDoesNotExist(ctx, shortName, emojiName); err != nil {
		return command.Result{}, err
	}

	channelGroupID, err := h.groupChecker.CreateChannelGroup(ctx, shortName, true)
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
			if err := h.mappingWriter.DeleteEmojiGroupMappingByShortName(ctx, shortName); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		if err := h.groupChecker.DeleteChannelGroup(ctx, channelGroupID); err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
		if len(rollbackErrs) == 0 {
			return cause
		}
		return fmt.Errorf("%w (rollback failed: %w)", cause, errors.Join(rollbackErrs...))
	}

	now := time.Now().UTC()
	if err := h.mappingWriter.UpsertEmojiGroupMapping(ctx, storage.EmojiGroupMapping{
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

func (h *GroupHandler) ensureMappingDoesNotExist(ctx context.Context, shortName, emojiName string) error {
	mappings, err := h.mappingReader.ListAllEmojiGroupMappings(ctx)
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
	args []string,
) (command.Result, error) {
	if len(args) != 1 {
		return command.Result{}, command.NewUserError("Usage: `group subscribe <course_short_name>`")
	}
	shortName := args[0]

	mapping, found, err := h.mappingReader.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(unknownGroupMessage(ctx, h.auth, req, shortName))
	}

	if err := h.subscriber.SubscribeUser(ctx, req.Actor.UserID, mapping.ChannelGroupID); err != nil {
		return command.Result{}, fmt.Errorf("subscribe user to group: %w", err)
	}

	return command.Result{
		Content: fmt.Sprintf("You have been subscribed to **%s**.", mapping.ShortName),
	}, nil
}

func (h *GroupHandler) handleUnsubscribe(
	ctx context.Context,
	req command.Request,
	args []string,
) (command.Result, error) {
	keepChannels := false
	remaining := args

	if len(remaining) >= 1 && remaining[0] == "-k" {
		keepChannels = true
		remaining = remaining[1:]
	}

	if len(remaining) != 1 {
		return command.Result{}, command.NewUserError("Usage: `group unsubscribe [-k] <course_short_name>`")
	}
	shortName := remaining[0]

	mapping, found, err := h.mappingReader.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(unknownGroupMessage(ctx, h.auth, req, shortName))
	}

	if keepChannels {
		if err := h.subscriber.UnsubscribeUserKeepChannels(ctx, req.Actor.UserID, mapping.ChannelGroupID); err != nil {
			return command.Result{}, fmt.Errorf("unsubscribe user from group (keep channels): %w", err)
		}
		return command.Result{
			Content: fmt.Sprintf("You have been unsubscribed from **%s** (channels kept).", mapping.ShortName),
		}, nil
	}

	if err := h.subscriber.UnsubscribeUser(ctx, req.Actor.UserID, mapping.ChannelGroupID); err != nil {
		return command.Result{}, fmt.Errorf("unsubscribe user from group: %w", err)
	}

	return command.Result{
		Content: fmt.Sprintf("You have been unsubscribed from **%s**.", mapping.ShortName),
	}, nil
}

// handleAvailable lists user groups visible in Zulip to the bot account. It is
// the admin-facing way to discover which Zulip user group IDs may be passed to
// `group mapping set`. Local-import state is intentionally not shown: the bot
// auto-imports on first mapping, so admins should not need to think about it.
func (h *GroupHandler) handleAvailable(ctx context.Context, req command.Request) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}

	groups, err := h.groupChecker.ListZulipUserGroups(ctx)
	if err != nil {
		return command.Result{}, fmt.Errorf("list zulip user groups: %w", err)
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
// Admins and owners may see a hint about `group mapping list`; normal users see a generic message.
func unknownGroupMessage(ctx context.Context, auth command.Authorizer, req command.Request, shortName string) string {
	if auth.Check(ctx, req.Actor, command.PermAdmin) == nil {
		return fmt.Sprintf("Unknown channel group %q. Use `group mapping list` to see available groups.", shortName)
	}
	return fmt.Sprintf(
		"Unknown channel group %q. Use `help group` to see the command format, or ask an admin to check available groups.",
		shortName,
	)
}

func (h *GroupHandler) handleMapping(ctx context.Context, req command.Request, args []string) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}

	if len(args) == 0 {
		return command.Result{}, command.NewUserError("Usage: `group mapping <list|set|disable> [args...]`")
	}

	switch args[0] {
	case "list":
		return h.handleMappingList(ctx)
	case "set":
		return h.handleMappingSet(ctx, args[1:])
	case "disable":
		return h.handleMappingDisable(ctx, req, args[1:])
	default:
		return command.Result{}, command.NewUserError(
			fmt.Sprintf("Unknown mapping subcommand %q. Use: list, set, disable", args[0]),
		)
	}
}

func (h *GroupHandler) handleMappingList(ctx context.Context) (command.Result, error) {
	mappings, err := h.mappingReader.ListAllEmojiGroupMappings(ctx)
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
		exists, checkErr := h.groupChecker.ChannelGroupExists(ctx, m.ChannelGroupID)
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
// missing channel group. Used to gate announcement publication so the bot does
// not advertise reactions it cannot fulfil.
func (h *GroupHandler) validateEnabledMappings(ctx context.Context) ([]storage.EmojiGroupMapping, error) {
	mappings, err := h.mappingReader.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return nil, err
	}
	var invalid []storage.EmojiGroupMapping
	for _, m := range mappings {
		exists, err := h.groupChecker.ChannelGroupExists(ctx, m.ChannelGroupID)
		if err != nil {
			return nil, fmt.Errorf("verify channel group %d exists: %w", m.ChannelGroupID, err)
		}
		if !exists {
			invalid = append(invalid, m)
		}
	}
	return invalid, nil
}

func (h *GroupHandler) handleMappingSet(ctx context.Context, args []string) (command.Result, error) {
	// Format: group mapping set <short_name> <zulip_user_group_id> <emoji_name>
	if len(args) != 3 { //nolint:mnd // Command requires short name, group ID, and emoji name.
		return command.Result{}, command.NewUserError(
			"Usage: `group mapping set <short_name> <zulip_user_group_id> <emoji_name>`",
		)
	}
	shortName := args[0]
	channelGroupIDStr := args[1]
	emojiName := args[2]

	channelGroupID, err := strconv.ParseInt(channelGroupIDStr, 10, 64)
	if err != nil || channelGroupID <= 0 {
		return command.Result{}, command.NewUserError("zulip_user_group_id must be a positive integer")
	}

	exists, err := h.groupChecker.ChannelGroupExists(ctx, channelGroupID)
	if err != nil {
		return command.Result{}, fmt.Errorf("verify channel group %d exists: %w", channelGroupID, err)
	}
	imported := false
	if !exists {
		// Auto-import: only proceed if the Zulip user group is visible to the bot.
		visible, visibleErr := h.zulipGroupVisible(ctx, channelGroupID)
		if visibleErr != nil {
			return command.Result{}, fmt.Errorf("check zulip visibility for group %d: %w", channelGroupID, visibleErr)
		}
		if !visible {
			return command.Result{}, command.NewUserError(fmt.Sprintf(
				"Channel group %d is not visible in Zulip. Run `group available` to see available groups.",
				channelGroupID,
			))
		}
		if importErr := h.groupChecker.ImportZulipUserGroup(ctx, channelGroupID); importErr != nil {
			return command.Result{}, fmt.Errorf("auto-import channel group %d: %w", channelGroupID, importErr)
		}
		imported = true
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
	if err := h.mappingWriter.UpsertEmojiGroupMapping(ctx, mapping); err != nil {
		return command.Result{}, fmt.Errorf("upsert emoji group mapping: %w", err)
	}

	// Trigger announcement update
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
// ID in Zulip. It uses the same source as `group available`.
func (h *GroupHandler) zulipGroupVisible(ctx context.Context, userGroupID int64) (bool, error) {
	groups, err := h.groupChecker.ListZulipUserGroups(ctx)
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

func (h *GroupHandler) handleMappingDisable(
	ctx context.Context,
	req command.Request,
	args []string,
) (command.Result, error) {
	_ = req
	if len(args) != 1 {
		return command.Result{}, command.NewUserError("Usage: `group mapping disable <short_name>`")
	}
	shortName := args[0]

	if err := h.mappingWriter.SetEmojiGroupMappingEnabled(ctx, shortName, false); err != nil {
		return command.Result{}, fmt.Errorf("disable emoji group mapping: %w", err)
	}

	// Trigger announcement update
	h.triggerAnnouncementUpdate(ctx)

	return command.Result{
		Content: fmt.Sprintf("Mapping `%s` disabled.", shortName),
	}, nil
}

func (h *GroupHandler) handleAnnounce(ctx context.Context, req command.Request, args []string) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}

	if len(args) == 0 {
		return h.runAnnounce(ctx)
	}

	switch args[0] {
	case "set-message":
		return h.handleAnnounceSetMessage(ctx, args[1:])
	case "inspect":
		return h.handleAnnounceInspect(ctx)
	default:
		return command.Result{}, command.NewUserError(
			fmt.Sprintf("Unknown announce subcommand %q. Use: set-message, inspect", args[0]),
		)
	}
}

func (h *GroupHandler) runAnnounce(ctx context.Context) (command.Result, error) {
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

	state, ok, err := h.stateAccessor.GetAnnouncementState(ctx)
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

func (h *GroupHandler) handleAnnounceSetMessage(ctx context.Context, args []string) (command.Result, error) {
	if len(args) != 1 {
		return command.Result{}, command.NewUserError("Usage: `group announce set-message <message_id>`")
	}
	msgID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || msgID <= 0 {
		return command.Result{}, command.NewUserError("message_id must be a positive integer")
	}

	if err := h.stateAccessor.SaveAnnouncementState(ctx, storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "", // force re-render on next announce
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

func (h *GroupHandler) handleAnnounceInspect(ctx context.Context) (command.Result, error) {
	state, ok, err := h.stateAccessor.GetAnnouncementState(ctx)
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

func (h *GroupHandler) handleChannel(ctx context.Context, req command.Request, args []string) (command.Result, error) {
	if err := h.auth.Check(ctx, req.Actor, command.PermAdmin); err != nil {
		return command.Result{}, command.NewUserError("permission denied")
	}
	if h.channelManager == nil {
		return command.Result{}, command.NewUserError("channel command not available")
	}
	if len(args) == 0 {
		return command.Result{}, command.NewUserError(
			"Usage: `group channel <add|remove|create> <channel_id_or_name> <short_name>`",
		)
	}
	switch args[0] {
	case "add":
		return h.handleChannelAdd(ctx, args[1:])
	case "remove":
		return h.handleChannelRemove(ctx, args[1:])
	case "create":
		return h.handleChannelCreate(ctx, args[1:])
	default:
		return command.Result{}, command.NewUserError(
			fmt.Sprintf("Unknown channel subcommand %q. Use: add, remove, create", args[0]),
		)
	}
}

func (h *GroupHandler) handleChannelModify(
	ctx context.Context,
	subcommand string,
	args []string,
	op func(ctx context.Context, groupID, channelID int64) error,
	successFmt string,
) (command.Result, error) {
	if len(args) != 2 { //nolint:mnd // Requires channel_id and short_name.
		return command.Result{}, command.NewUserError(
			fmt.Sprintf("Usage: `group channel %s <channel_id> <short_name>`", subcommand),
		)
	}
	channelID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || channelID <= 0 {
		return command.Result{}, command.NewUserError("channel_id must be a positive integer")
	}
	shortName := args[1]

	mapping, found, err := h.mappingReader.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	if err := op(ctx, mapping.ChannelGroupID, channelID); err != nil {
		return command.Result{}, fmt.Errorf("%s channel in group: %w", subcommand, err)
	}
	return command.Result{Content: fmt.Sprintf(successFmt, channelID, mapping.ShortName)}, nil
}

func (h *GroupHandler) handleChannelAdd(ctx context.Context, args []string) (command.Result, error) {
	return h.handleChannelModify(ctx, "add", args, h.channelManager.AddChannelToGroup,
		"Added channel %d to **%s**.")
}

func (h *GroupHandler) handleChannelRemove(ctx context.Context, args []string) (command.Result, error) {
	return h.handleChannelModify(ctx, "remove", args, h.channelManager.RemoveChannelFromGroup,
		"Removed channel %d from **%s**.")
}

func (h *GroupHandler) handleChannelCreate(ctx context.Context, args []string) (command.Result, error) {
	if len(args) != 2 { //nolint:mnd // Requires channel_name and short_name.
		return command.Result{}, command.NewUserError("Usage: `group channel create <channel_name> <short_name>`")
	}
	channelName := args[0]
	shortName := args[1]

	mapping, found, err := h.mappingReader.GetEmojiGroupMappingByShortName(ctx, shortName)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Unknown channel group %q.", shortName))
	}

	channelID, err := h.channelManager.CreateChannelAndAddToGroup(ctx, channelName, mapping.ChannelGroupID)
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

func (h *GroupHandler) triggerAnnouncementUpdate(ctx context.Context) {
	state, ok, err := h.stateAccessor.GetAnnouncementState(ctx)
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
	// send is nil when message_id is stored; manager handles edit path

	if err := h.announcer.UpdateAfterMappingChange(ctx, send); err != nil {
		_ = err
	}
}

// Ensure GroupHandler implements command.Handler.
var _ command.Handler = (*GroupHandler)(nil)

// GroupConfigAdapterFromConfigSvc creates a GroupConfigReader backed by a raw config reader.
// This is used to adapt configsvc.Service to the GroupConfigReader interface.
type GroupConfigAdapter struct {
	getChannelID func(ctx context.Context) (int64, bool, error)
	getTopic     func(ctx context.Context) (string, bool, error)
}

// AnnouncementChannelID implements GroupConfigReader.
func (a *GroupConfigAdapter) AnnouncementChannelID(ctx context.Context) (int64, bool, error) {
	return a.getChannelID(ctx)
}

// AnnouncementTopic implements GroupConfigReader.
func (a *GroupConfigAdapter) AnnouncementTopic(ctx context.Context) (string, bool, error) {
	return a.getTopic(ctx)
}

// NewGroupConfigAdapter wraps two provider functions into a GroupConfigReader.
func NewGroupConfigAdapter(
	getChannelID func(ctx context.Context) (int64, bool, error),
	getTopic func(ctx context.Context) (string, bool, error),
) *GroupConfigAdapter {
	return &GroupConfigAdapter{
		getChannelID: getChannelID,
		getTopic:     getTopic,
	}
}

// groupConfigInterfaceCheck asserts GroupConfigAdapter implements GroupConfigReader.
var _ GroupConfigReader = (*GroupConfigAdapter)(nil)

// Ensure zulip.Role import is used.
var _ zulip.Role = command.PermAdmin
