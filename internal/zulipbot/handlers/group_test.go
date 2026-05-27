package handlers_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	z "github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

// --- Fakes for non-Zulip dependencies (auth) ---

// setAnnouncementConfig persists the announcement channel/topic config so the
// handler's storage-backed lookups return them. Pass channelID<=0 or topic=""
// to leave that key unset.
func setAnnouncementConfig(
	t *testing.T,
	queries *storagedb.Queries,
	channelID int64,
	topic string,
) {
	t.Helper()
	if channelID > 0 {
		if err := queries.SetConfigValue(context.Background(), storagedb.SetConfigValueParams{
			Key:       handlers.KeyAnnouncementChannelID,
			Value:     strconv.FormatInt(channelID, 10),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("SetConfigValue(channel_id): %v", err)
		}
	}
	if topic != "" {
		if err := queries.SetConfigValue(context.Background(), storagedb.SetConfigValueParams{
			Key:       handlers.KeyAnnouncementTopic,
			Value:     topic,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("SetConfigValue(topic): %v", err)
		}
	}
}

type allowAll struct{}

func (allowAll) Check(_ context.Context, _ command.Actor, _ z.Role) error { return nil }

type denyAll struct{}

func (denyAll) Check(_ context.Context, _ command.Actor, _ z.Role) error {
	return command.ErrDenied
}

// --- Common test helpers ---

func openGroupTestStorage(t *testing.T) (*sql.DB, *storagedb.Queries) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open() failed: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := storagedb.ConfigureSQLite(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("ConfigureSQLite() failed: %v", err)
	}
	if err := storagedb.InitSchema(context.Background(), db); err != nil {
		_ = db.Close()
		t.Fatalf("InitSchema() failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, storagedb.New(db)
}

func seedGroupMapping(
	t *testing.T,
	queries *storagedb.Queries,
	shortName, emojiName string,
	channelGroupID int64,
) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := queries.UpsertEmojiGroupMapping(
		context.Background(),
		storagedb.UpsertEmojiGroupMappingParams{
			ChannelGroupID: channelGroupID,
			EmojiName:      emojiName,
			Enabled:        1,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	)
	if err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}
}

func getGroupMappingByShortName(
	ctx context.Context,
	queries *storagedb.Queries,
	shortName string,
) (storagedb.EmojiGroupMapping, bool, error) {
	rows, err := queries.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		return storagedb.EmojiGroupMapping{}, false, err
	}
	if len(rows) == 1 {
		return rows[0], true, nil
	}
	row, err := queries.GetEmojiGroupMappingByChannelGroupID(ctx, 0)
	if errors.Is(err, sql.ErrNoRows) {
		return storagedb.EmojiGroupMapping{}, false, nil
	}
	return row, err == nil, err
}

func saveAnnouncementState(
	ctx context.Context,
	queries *storagedb.Queries,
	messageID *int64,
) error {
	var id sql.NullInt64
	if messageID != nil {
		id = sql.NullInt64{Int64: *messageID, Valid: true}
	}
	return queries.SaveAnnouncementState(ctx, storagedb.SaveAnnouncementStateParams{
		MessageID: id,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func makeGroupRequest(parsedArgs any) command.Request {
	return command.Request{
		ParsedArgs: parsedArgs,
		Actor:      command.Actor{UserID: 123},
		MessageID:  1,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{123}},
	}
}

func containsInt64(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func hasReaction(reactions []z.EmojiReaction, emojiName string, userID int64) bool {
	for _, reaction := range reactions {
		if reaction.EmojiName == emojiName && reaction.UserID == userID {
			return true
		}
	}
	return false
}

// newChannelGroupClient builds a channelgroup.Client backed by a fresh
// zulipmock base. The returned base may be used to seed user groups, set the
// bot's own user, or inject failures (FailNext).
func newChannelGroupClient(t *testing.T) (channelgroup.Client, zulipmock.Client) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := channelgroup.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate channelgroup schema: %v", err)
	}
	base := zulipmock.NewClient()
	base.SetOwnUser(z.User{UserID: 77, Email: "bot@example.com", FullName: "Bot", IsBot: true})
	client, err := channelgroup.NewClient(
		context.Background(),
		base,
		db,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("channelgroup.NewClient: %v", err)
	}
	return client, base
}

// seedChannelGroup creates a Zulip user group and imports it locally so the
// returned ID is fully usable as a channel group (channelGroupExists returns
// true, channel and subscriber operations succeed).
func seedChannelGroup(
	t *testing.T,
	client channelgroup.Client,
	base zulipmock.Client,
	name string,
) int64 {
	t.Helper()
	resp, _, err := base.CreateUserGroup(context.Background()).
		Name(name).
		Description("").
		Members([]int64{77}).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup(%q): %v", name, err)
	}
	if err := client.ImportZulipUserGroup(context.Background(), resp.GroupID); err != nil {
		t.Fatalf("ImportZulipUserGroup(%d): %v", resp.GroupID, err)
	}
	return resp.GroupID
}

// seedZulipUserGroup creates a user group in the Zulip mock so that it shows up
// in client.GetUserGroups. Returns the new group ID.
func seedZulipUserGroup(t *testing.T, base zulipmock.Client, name string, members []int64) int64 {
	t.Helper()
	resp, _, err := base.CreateUserGroup(context.Background()).
		Name(name).
		Description("").
		Members(members).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup(%q): %v", name, err)
	}
	return resp.GroupID
}

type groupTestEnv struct {
	db      *sql.DB
	queries *storagedb.Queries
	client  channelgroup.Client
	base    zulipmock.Client
}

type groupArgResolver struct {
	zulipmock.Client
}

func (r groupArgResolver) RenderMessage(ctx context.Context, content string) (string, error) {
	resp, _, err := r.Client.RenderMessage(ctx).Content(content).Execute()
	if err != nil {
		return "", err
	}
	return resp.Rendered, nil
}

func (r groupArgResolver) GetUserByID(ctx context.Context, id int64) (z.User, error) {
	resp, _, err := r.Client.GetUser(ctx, id).Execute()
	if err != nil {
		return z.User{}, err
	}
	return resp.User, nil
}

func (r groupArgResolver) GetChannelByID(ctx context.Context, id int64) (z.Channel, error) {
	resp, _, err := r.Client.GetChannelByID(ctx, id).Execute()
	if err != nil {
		return z.Channel{}, err
	}
	return resp.Channel, nil
}

func newGroupTestEnv(t *testing.T) *groupTestEnv {
	t.Helper()
	client, base := newChannelGroupClient(t)
	db, queries := openGroupTestStorage(t)
	return &groupTestEnv{
		db:      db,
		queries: queries,
		client:  client,
		base:    base,
	}
}

func (e *groupTestEnv) handler(auth command.Authorizer) *handlers.GroupHandler {
	return handlers.NewGroupHandler(e.client, e.queries, auth, nil)
}

// announcementHash returns the rendered-content hash currently stored, or "" if
// no announcement state exists yet. Tests use this as a proxy for "did the
// handler run an announcement send/edit?": the hash is only saved by
// ensureAnnouncement after a successful SendMessage/UpdateMessage call.
func announcementHash(t *testing.T, queries *storagedb.Queries) string {
	t.Helper()
	state, err := queries.GetAnnouncementState(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("GetAnnouncementState: %v", err)
	}
	return state.ContentHash
}

// --- Tests ---

func TestGroupSubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := env.client.GetIsChannelGroupSubscriber(ctx, groupID, 123).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber: %v", err)
	}
	if !resp.IsSubscriber {
		t.Errorf("expected actor 123 to be subscribed to group %d", groupID)
	}
}

func TestGroupSubscribeAlreadySubscribed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	if _, _, err := env.client.SubscribeToChannelGroup(ctx, groupID).
		Principals(z.Principals{UserIDs: &[]int64{123}}).Execute(); err != nil {
		t.Fatalf("pre-subscribe: %v", err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "already subscribed") {
		t.Errorf("expected already subscribed message, got: %s", result.Content)
	}
}

func TestGroupUnsubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	// Pre-subscribe the actor so unsubscribe has something to do.
	if _, _, err := env.client.SubscribeToChannelGroup(ctx, groupID).
		Principals(z.Principals{UserIDs: &[]int64{123}}).Execute(); err != nil {
		t.Fatalf("pre-subscribe: %v", err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupUnsubscribeArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := env.client.GetIsChannelGroupSubscriber(ctx, groupID, 123).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber: %v", err)
	}
	if resp.IsSubscriber {
		t.Errorf("expected actor 123 to be unsubscribed from group %d", groupID)
	}
}

func TestGroupUnsubscribeNotSubscribed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupUnsubscribeArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "not subscribed") {
		t.Errorf("expected not subscribed message, got: %s", result.Content)
	}
}

func TestGroupUnsubscribeKeepChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	if _, _, err := env.client.SubscribeToChannelGroup(ctx, groupID).
		Principals(z.Principals{UserIDs: &[]int64{123}}).Execute(); err != nil {
		t.Fatalf("pre-subscribe: %v", err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupUnsubscribeArgs{KeepChannels: true, ShortName: "WI"}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "channels kept") {
		t.Errorf("expected 'channels kept' in result, got: %s", result.Content)
	}
	resp, _, err := env.client.GetIsChannelGroupSubscriber(ctx, groupID, 123).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber: %v", err)
	}
	if resp.IsSubscriber {
		t.Errorf("expected actor 123 to be unsubscribed from group %d", groupID)
	}
}

func TestGroupSubscribeUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "UNKNOWN"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for unknown group, got %T: %v", err, err)
	}
}

func TestGroupSubscribeUnknownGroupNoneUserDoesNotLeakAdminHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(denyAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "UNKNOWN"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for unknown group, got %T: %v", err, err)
	}
	if strings.Contains(userErr.Message, "group mapping list") {
		t.Errorf(
			"normal user error must not mention 'group mapping list', got: %q",
			userErr.Message,
		)
	}
	if !strings.Contains(userErr.Message, "help group") &&
		!strings.Contains(userErr.Message, "admin") {
		t.Errorf("normal user error should suggest help or admin, got: %q", userErr.Message)
	}
}

func TestGroupSubscribeUnknownGroupAdminSeesHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "UNKNOWN"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for unknown group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "group mapping list") {
		t.Errorf("admin error should mention 'group mapping list', got: %q", userErr.Message)
	}
}

func TestGroupUnsubscribeUnknownGroupNoneUserDoesNotLeakAdminHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(denyAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupUnsubscribeArgs{ShortName: "UNKNOWN"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for unknown group, got %T: %v", err, err)
	}
	if strings.Contains(userErr.Message, "group mapping list") {
		t.Errorf(
			"normal user unsubscribe error must not mention 'group mapping list', got: %q",
			userErr.Message,
		)
	}
}

func TestGroupListIsNoLongerRecognised(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parser := command.NewArgParser(nil)
	_, err := parser.Parse(ctx, handlers.GroupArgSpec, []string{"list"})
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for removed `group list` subcommand, got %T: %v", err, err)
	}
	if !strings.Contains(strings.ToLower(userErr.Message), "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' message, got: %q", userErr.Message)
	}
}

func TestGroupCreateAdminCreatesChannelGroupAndMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupCreateArgs{ShortName: "PGDP", EmojiName: ":books:"}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	mapping, ok, err := getGroupMappingByShortName(ctx, env.queries, "PGDP")
	if err != nil || !ok {
		t.Fatalf("expected created mapping, ok=%v err=%v", ok, err)
	}
	if mapping.ChannelGroupID <= 0 || mapping.EmojiName != "books" {
		t.Fatalf("unexpected mapping: %+v", mapping)
	}
	// The created channel group should exist locally.
	if _, _, err := env.client.GetChannelGroup(ctx, mapping.ChannelGroupID).Execute(); err != nil {
		t.Errorf(
			"expected channel group %d to exist after create, got %v",
			mapping.ChannelGroupID,
			err,
		)
	}
	if !strings.Contains(result.Content, "PGDP") || !strings.Contains(result.Content, "books") {
		t.Errorf("expected created group response with name and emoji, got: %s", result.Content)
	}
}

func TestGroupRemoveEmptyGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupRemoveArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "Removed channel group") {
		t.Fatalf("unexpected result content: %q", result.Content)
	}
	if _, _, err = env.client.GetChannelGroup(ctx, groupID).Execute(); !errors.Is(
		err,
		channelgroup.ErrChannelGroupNotFound,
	) {
		t.Fatalf("GetChannelGroup error = %v, want ErrChannelGroupNotFound", err)
	}
	if _, found, err := getGroupMappingByShortName(ctx, env.queries, "WI"); err != nil || found {
		t.Fatalf("mapping found = %t, err = %v; want not found", found, err)
	}
}

func TestGroupRemoveRejectsAssignedChannelsWithoutForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupRemoveArgs{ShortName: "WI"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "group remove -f WI") {
		t.Fatalf("expected force hint, got %q", userErr.Message)
	}
	if _, _, err = env.client.GetChannelGroup(ctx, groupID).Execute(); err != nil {
		t.Fatalf("group should still exist: %v", err)
	}
}

func TestGroupRemoveForceArchivesChannelsAndFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	if _, _, err := env.client.UpdateChannelGroupFolder(ctx, groupID).Add().Execute(); err != nil {
		t.Fatalf("pre-add folder to group %d: %v", groupID, err)
	}
	group, _, err := env.client.GetChannelGroup(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup: %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after group folder add")
	}
	folderID := *group.ChannelGroup.ChannelFolderID

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupRemoveArgs{Force: true, ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "archived 1 exclusive channel") {
		t.Fatalf("unexpected result content: %q", result.Content)
	}
	if _, _, err = env.client.GetChannelGroup(ctx, groupID).Execute(); !errors.Is(
		err,
		channelgroup.ErrChannelGroupNotFound,
	) {
		t.Fatalf("GetChannelGroup error = %v, want ErrChannelGroupNotFound", err)
	}
	if _, found, err := getGroupMappingByShortName(ctx, env.queries, "WI"); err != nil || found {
		t.Fatalf("mapping found = %t, err = %v; want not found", found, err)
	}
	channel, _, err := env.base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if !channel.Channel.IsArchived {
		t.Fatal("channel was not archived")
	}
	if channel.Channel.FolderID != nil {
		t.Fatalf("channel folder ID = %v, want nil", channel.Channel.FolderID)
	}
	folders, _, err := env.base.GetChannelFolders(ctx).IncludeArchived(true).Execute()
	if err != nil {
		t.Fatalf("GetChannelFolders: %v", err)
	}
	for _, folder := range folders.ChannelFolders {
		if folder.ID == folderID {
			if !folder.IsArchived {
				t.Fatalf("folder %d was not archived", folderID)
			}
			return
		}
	}
	t.Fatalf("folder %d not found", folderID)
}

func TestGroupRemoveForceDoesNotArchiveChannelsSharedWithOtherGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	otherGroupID := seedChannelGroup(t, env.client, env.base, "Other")
	channelID := seedChannel(t, env.base, "shared-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, otherGroupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, otherGroupID, err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupRemoveArgs{Force: true, ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "archived 0 exclusive channel") {
		t.Fatalf("unexpected result content: %q", result.Content)
	}
	channel, _, err := env.base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.IsArchived {
		t.Fatal("shared channel was archived")
	}
	otherGroup, _, err := env.client.GetChannelGroup(ctx, otherGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup(other): %v", err)
	}
	if !containsInt64(otherGroup.ChannelGroup.ChannelIDs, channelID) {
		t.Fatalf("shared channel %d was removed from other group %+v", channelID, otherGroup.ChannelGroup)
	}
	if _, found, err := getGroupMappingByShortName(ctx, env.queries, "WI"); err != nil || found {
		t.Fatalf("mapping found = %t, err = %v; want not found", found, err)
	}
}

func TestGroupCreateRequiresColonWrappedEmojiName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupCreateArgs{ShortName: "PGDP", EmojiName: "books"}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for bare emoji name, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "<:emoji_name:>") {
		t.Fatalf("expected colon-wrapped emoji guidance, got: %q", userErr.Message)
	}
	if _, ok, _ := getGroupMappingByShortName(ctx, env.queries, "PGDP"); ok {
		t.Fatal("expected no mapping created for bare emoji name")
	}
}

func TestGroupCreateDuplicateZulipUserGroupReturnsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	env.base.FailNext(zulipmock.OperationCreateUserGroup, z.CodedError{
		Response: z.Response{Result: z.ResponseError, Msg: "User group 'pgdp2' already exists."},
		Code:     "BAD_REQUEST",
	})

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupCreateArgs{ShortName: "pgdp2", EmojiName: ":books:"}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for duplicate group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "already exists") ||
		!strings.Contains(userErr.Message, "group mapping set") {
		t.Fatalf("expected duplicate group guidance, got: %q", userErr.Message)
	}
}

func TestGroupCreateReusesArchivedZulipUserGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "ONE", []int64{1})
	if _, _, err := env.base.DeactivateUserGroup(ctx, groupID).Execute(); err != nil {
		t.Fatalf("DeactivateUserGroup(%d): %v", groupID, err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupCreateArgs{ShortName: "ONE", EmojiName: ":books:"}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if strings.Contains(result.Content, "group mapping set") {
		t.Fatalf("expected archived group reuse, got duplicate guidance: %q", result.Content)
	}
	mapping, ok, err := getGroupMappingByShortName(ctx, env.queries, "ONE")
	if err != nil {
		t.Fatalf("get mapping: %v", err)
	}
	if !ok {
		t.Fatal("expected mapping for reused archived group")
	}
	if mapping.ChannelGroupID != groupID {
		t.Fatalf("mapping channel group ID = %d, want %d", mapping.ChannelGroupID, groupID)
	}
	groups, _, err := env.base.GetUserGroups(ctx).IncludeDeactivatedGroups(true).Execute()
	if err != nil {
		t.Fatalf("GetUserGroups: %v", err)
	}
	for _, group := range groups.UserGroups {
		if group.ID == groupID && group.Deactivated {
			t.Fatalf("reused group %d is still deactivated", groupID)
		}
	}
}

func TestGroupCreateDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(denyAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupCreateArgs{ShortName: "PGDP", EmojiName: ":books:"}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for denied create, got %T: %v", err, err)
	}
	if _, ok, _ := getGroupMappingByShortName(ctx, env.queries, "PGDP"); ok {
		t.Fatal("expected no mapping created when permission denied")
	}
}

func TestGroupMappingSetAutoImportsWhenZulipVisibleButNotLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "PGDP", []int64{1})
	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{
			ShortName:  "PGDP",
			ZulipGroup: z.User{UserID: groupID, FullName: "PGDP"},
			EmojiName:  ":math:",
		}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf("success message should mention auto-import, got: %q", result.Content)
	}
	// Local channel group now exists.
	if _, _, err := env.client.GetChannelGroup(ctx, groupID).Execute(); err != nil {
		t.Errorf(
			"expected channel group %d to exist locally after auto-import, got %v",
			groupID,
			err,
		)
	}
	m, ok, err := getGroupMappingByShortName(ctx, env.queries, "PGDP")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf(
			"expected mapping stored with channel group %d, got m=%+v ok=%v err=%v",
			groupID,
			m,
			ok,
			err,
		)
	}
	if announcementHash(t, env.queries) == "" {
		t.Error("expected announcement message to be updated after successful mapping set")
	}
}

func TestGroupMappingSetParsesUserGroupMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "PGDP", []int64{1})
	parser := command.NewArgParser(groupArgResolver{Client: env.base})

	parsed, err := parser.Parse(
		ctx,
		handlers.GroupArgSpec,
		[]string{"mapping", "set", "PGDP", "@**PGDP**", ":math:"},
	)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	args, ok := parsed.(handlers.GroupMappingSetArgs)
	if !ok {
		t.Fatalf("expected GroupMappingSetArgs, got %T", parsed)
	}
	if args.ZulipGroup.UserID != groupID || args.ZulipGroup.FullName != "PGDP" {
		t.Fatalf("unexpected ZulipGroup: %+v", args.ZulipGroup)
	}
}

func TestGroupMappingSetRejectsNumericUserGroupID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "PGDP", []int64{1})
	parser := command.NewArgParser(groupArgResolver{Client: env.base})

	_, err := parser.Parse(
		ctx,
		handlers.GroupArgSpec,
		[]string{"mapping", "set", "PGDP", itoa(groupID), ":math:"},
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Zulip user mention") {
		t.Fatalf("expected mention-only error, got %q", userErr.Message)
	}
}

func TestGroupMappingSetRequiresColonWrappedEmojiName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "PGDP", []int64{1})

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{
			ShortName:  "PGDP",
			ZulipGroup: z.User{UserID: groupID, FullName: "PGDP"},
			EmojiName:  "math",
		}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for bare emoji name, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "<:emoji_name:>") {
		t.Fatalf("expected colon-wrapped emoji guidance, got: %q", userErr.Message)
	}
	if _, ok, err := getGroupMappingByShortName(ctx, env.queries, "PGDP"); err != nil || ok {
		t.Fatalf("expected no PGDP mapping, ok=%v err=%v", ok, err)
	}
}

func TestGroupMappingSetRejectsDuplicateEnabledEmoji(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	existingGroupID := seedZulipUserGroup(t, env.base, "PGDP", []int64{1})
	newGroupID := seedZulipUserGroup(t, env.base, "GAD", []int64{1})
	seedGroupMapping(t, env.queries, "PGDP", "math", existingGroupID)

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{
			ShortName:  "GAD",
			ZulipGroup: z.User{UserID: newGroupID, FullName: "GAD"},
			EmojiName:  ":math:",
		}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for duplicate emoji, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "already mapped to `channel_group_id:100`") {
		t.Fatalf("expected duplicate emoji guidance, got: %q", userErr.Message)
	}
	mappings, err := env.queries.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		t.Fatalf("ListAllEmojiGroupMappings() failed: %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("expected no additional GAD mapping, got %d mappings", len(mappings))
	}
}

func TestGroupMappingSetSkipsAutoImportWhenAlreadyLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "NEWCOURSE")
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupMappingSetArgs{
				ShortName:  "NEWCOURSE",
				ZulipGroup: z.User{UserID: groupID, FullName: "NEWCOURSE"},
				EmojiName:  ":newemoji:",
			},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf(
			"success message must not mention import when no import happened, got: %q",
			result.Content,
		)
	}
	m, ok, err := getGroupMappingByShortName(ctx, env.queries, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf(
			"expected mapping stored with channel group %d, got m=%+v ok=%v err=%v",
			groupID,
			m,
			ok,
			err,
		)
	}
}

func TestGroupMappingSetRejectsWhenZulipDoesNotKnowGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	seedZulipUserGroup(t, env.base, "OtherGroup", []int64{1})
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	// Use an ID that does not match any seeded group.
	bogusID := int64(999999)
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{
			ShortName:  "PGDP",
			ZulipGroup: z.User{UserID: bogusID, FullName: "PGDP"},
			EmojiName:  ":math:",
		}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for invisible Zulip group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "not a visible Zulip user group") {
		t.Errorf(
			"error should say group is not a visible Zulip user group, got: %q",
			userErr.Message,
		)
	}
	if strings.Contains(userErr.Message, "group available") {
		t.Errorf(
			"error should not hint admins to run removed `group available`, got: %q",
			userErr.Message,
		)
	}
	if _, ok, err := getGroupMappingByShortName(ctx, env.queries, "PGDP"); err != nil || ok {
		t.Errorf("expected mapping not to be stored, got err=%v, ok=%v", err, ok)
	}
	if env.base.LastSentMessage() != nil {
		t.Errorf(
			"expected no message sent on rejected mapping, got %+v",
			env.base.LastSentMessage(),
		)
	}
}

func TestGroupMappingSetRejectsPlainUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	env.base.AddUser(z.User{UserID: 808, FullName: "Plain User", Email: "plain@example.com"})
	seedZulipUserGroup(t, env.base, "OtherGroup", []int64{1})
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{
			ShortName:  "PGDP",
			ZulipGroup: z.User{UserID: 808, FullName: "Plain User"},
			EmojiName:  ":math:",
		}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for plain user, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "not a visible Zulip user group") {
		t.Errorf("error should reject non-group user, got: %q", userErr.Message)
	}
	if _, ok, err := getGroupMappingByShortName(ctx, env.queries, "PGDP"); err != nil || ok {
		t.Errorf("expected mapping not to be stored, got err=%v, ok=%v", err, ok)
	}
}

func TestGroupMappingSetAcceptsExistingChannelGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "NEWCOURSE")
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupMappingSetArgs{
				ShortName:  "NEWCOURSE",
				ZulipGroup: z.User{UserID: groupID, FullName: "NEWCOURSE"},
				EmojiName:  ":newemoji:",
			},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	m, ok, err := getGroupMappingByShortName(ctx, env.queries, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf(
			"expected mapping stored with channel group %d, got m=%+v ok=%v err=%v",
			groupID,
			m,
			ok,
			err,
		)
	}
}

func TestGroupMappingListAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupMappingListArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty list output")
	}
}

func TestGroupMappingListDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(denyAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupMappingListArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for denied access, got %T: %v", err, err)
	}
}

func TestGroupAnnounceNoConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError when config not set, got %T: %v", err, err)
	}
}

func TestGroupAnnounceRejectsInvalidEnabledMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	// PGDP references a missing channel group 9999; WI references the imported group.
	wiID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "PGDP", "pgdp", 9999)
	seedGroupMapping(t, env.queries, "WI", "wi", wiID)

	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf(
			"expected UserError when an enabled mapping references a missing channel group, got %T: %v",
			err,
			err,
		)
	}
	if !strings.Contains(userErr.Message, "channel_group_id:9999") || !strings.Contains(userErr.Message, "9999") {
		t.Errorf("error should list invalid mapping channel_group_id:9999/9999, got: %q", userErr.Message)
	}
	if got := announcementHash(t, env.queries); got != "" {
		t.Errorf("expected no announcement update when validation fails, got hash %q", got)
	}
}

func TestGroupAnnounceIgnoresDisabledInvalidMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	wiID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "PGDP", "pgdp", 9999)
	if err := env.queries.SetEmojiGroupMappingEnabled(ctx, storagedb.SetEmojiGroupMappingEnabledParams{
		Enabled:        0,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		ChannelGroupID: 9999,
	}); err != nil {
		t.Fatalf("disable mapping: %v", err)
	}
	seedGroupMapping(t, env.queries, "WI", "wi", wiID)
	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	if _, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{})); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcementHash(t, env.queries) == "" {
		t.Error("expected announcement message to be updated when only disabled mapping is invalid")
	}
}

func TestGroupAnnounceAllValidMappings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.queries, 1, "t")

	h := env.handler(allowAll{})
	if _, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{})); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcementHash(t, env.queries) == "" {
		t.Error("expected announcement message to be updated for valid mappings")
	}
}

func TestGroupAnnounceSyncsBotReactions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	env.base.SetMessageReactions(msgID, []z.EmojiReaction{
		{EmojiName: "old", UserID: 77},
		{EmojiName: "old", UserID: 123},
	})

	h := env.handler(allowAll{})
	if _, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{})); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}

	reactions := env.base.MessageReactions(msgID)
	if !hasReaction(reactions, "wi", 77) {
		t.Fatalf("expected bot reaction :wi: to be added, got %#v", reactions)
	}
	if hasReaction(reactions, "old", 77) {
		t.Fatalf("expected stale bot reaction :old: to be removed, got %#v", reactions)
	}
	if !hasReaction(reactions, "old", 123) {
		t.Fatalf("expected other user's :old: reaction to remain, got %#v", reactions)
	}
}

func TestGroupMappingListAnnotatesMissingChannelGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	wiID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "PGDP", "pgdp", 9999)
	seedGroupMapping(t, env.queries, "WI", "wi", wiID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupMappingListArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "channel_group_id:9999") ||
		!strings.Contains(result.Content, "missing channel group") {
		t.Errorf("expected missing channel group to be flagged, got:\n%s", result.Content)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		if strings.Contains(line, "`WI`") && strings.Contains(line, "missing channel group") {
			t.Errorf("expected WI not to be flagged as missing, got:\n%s", result.Content)
		}
	}
}

func TestGroupInvalidSubcommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parser := command.NewArgParser(nil)
	_, err := parser.Parse(ctx, handlers.GroupArgSpec, []string{"badcmd"})
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for invalid subcommand, got %T: %v", err, err)
	}
}

func TestGroupAnnounceExistingMessageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	msgID := int64(555)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
	if announcementHash(t, env.queries) == "" {
		t.Error("expected announcement message to be updated")
	}
}

func TestGroupAnnounceNoConfigNoMessageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError when no config and no message_id, got %T: %v", err, err)
	}
}

func TestGroupAnnounceSetMessageValid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupAnnounceSetMessageArgs{MessageID: 12345}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
	state, err := env.queries.GetAnnouncementState(ctx)
	if err != nil || !state.MessageID.Valid || state.MessageID.Int64 != 12345 {
		t.Errorf("expected message_id 12345, got state=%+v err=%v", state, err)
	}
}

func TestGroupAnnounceSetMessageInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	h := env.handler(allowAll{})

	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceSetMessageArgs{MessageID: 0}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for 0, got %T: %v", err, err)
	}

	_, err = h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceSetMessageArgs{MessageID: -1}))
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for -1, got %T: %v", err, err)
	}

	parser := command.NewArgParser(nil)
	_, err = parser.Parse(ctx, handlers.GroupArgSpec, []string{"announce", "set-message", "abc"})
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for abc, got %T: %v", err, err)
	}
}

func TestGroupMetadataAdminUsageIsSet(t *testing.T) {
	t.Parallel()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	gh := env.handler(allowAll{})
	meta := gh.Metadata()
	if meta.AdminUsage == "" {
		t.Fatal("GroupHandler.Metadata().AdminUsage must not be empty")
	}
	if !strings.Contains(meta.AdminUsage, "mapping") {
		t.Errorf("AdminUsage should mention 'mapping', got: %q", meta.AdminUsage)
	}
	if !strings.Contains(meta.AdminUsage, "announce") {
		t.Errorf("AdminUsage should mention 'announce', got: %q", meta.AdminUsage)
	}
	if strings.Contains(meta.AdminUsage, "group available") {
		t.Errorf(
			"AdminUsage must NOT mention removed 'group available' command, got: %q",
			meta.AdminUsage,
		)
	}
	if strings.Contains(meta.AdminUsage, "group list") {
		t.Errorf(
			"AdminUsage must NOT mention removed 'group list' command, got: %q",
			meta.AdminUsage,
		)
	}
}

func TestGroupAdminCommandDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	h := env.handler(denyAll{})

	for _, tc := range []struct {
		name       string
		parsedArgs any
	}{
		{"mapping_list", handlers.GroupMappingListArgs{}},
		{"announce", handlers.GroupAnnounceArgs{}},
		{"announce_inspect", handlers.GroupAnnounceInspectArgs{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(ctx, makeGroupRequest(tc.parsedArgs))
			var userErr command.UserError
			if !errors.As(err, &userErr) {
				t.Errorf("expected UserError for denied %v, got %T: %v", tc.name, err, err)
			}
		})
	}
}

func TestGroupSubscribeStillWorksForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupSubscribeArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("subscribe should succeed for any user, got: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
}

func TestGroupAnnounceInspect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	msgID := int64(999)
	if err := saveAnnouncementState(ctx, env.queries, &msgID); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.queries, 42, "mytopic")

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceInspectArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty inspect output")
	}
	if !strings.Contains(result.Content, "999") {
		t.Errorf("expected inspect output to contain message_id 999, got: %s", result.Content)
	}
}

// --- Channel (`group channel ...`) subcommand tests ---

func newCourseTestEnv(t *testing.T) (*groupTestEnv, int64) {
	t.Helper()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.queries, "WI", "wi", groupID)
	return env, groupID
}

func seedChannel(t *testing.T, base zulipmock.Client, name string) int64 {
	t.Helper()
	resp, _, err := base.CreateChannel(context.Background()).Name(name).Execute()
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", name, err)
	}
	return resp.ID
}

func TestGroupCourseAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	h := env.handler(allowAll{})

	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupChannelAddArgs{
		Channel:   z.Channel{ChannelID: channelID},
		ShortName: "WI",
	}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := env.client.GetIsChannelInChannelGroup(ctx, groupID, channelID).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelInChannelGroup: %v", err)
	}
	if !resp.IsChannelGroupMember {
		t.Errorf("expected channel %d to be in group %d after add", channelID, groupID)
	}
}

func TestGroupCourseRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")

	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupChannelRemoveArgs{
			Channel:   z.Channel{ChannelID: channelID},
			ShortName: "WI",
		}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := env.client.GetIsChannelInChannelGroup(ctx, groupID, channelID).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelInChannelGroup: %v", err)
	}
	if resp.IsChannelGroupMember {
		t.Errorf("expected channel %d to not be in group %d after remove", channelID, groupID)
	}
}

func TestGroupCoursePermissionDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	h := env.handler(denyAll{})

	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupChannelAddArgs{
		Channel:   z.Channel{ChannelID: 99},
		ShortName: "WI",
	}))
	if err == nil {
		t.Fatal("expected error for non-admin user")
	}
}

func TestGroupCourseUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	h := env.handler(allowAll{})

	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupChannelAddArgs{
		Channel:   z.Channel{ChannelID: 99},
		ShortName: "UNKNOWN",
	}))
	if err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestGroupCourseInvalidChannelID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parser := command.NewArgParser(nil)
	_, err := parser.Parse(ctx, handlers.GroupArgSpec, []string{"course", "add", "notanint", "WI"})
	if err == nil {
		t.Fatal("expected error for invalid channel_id")
	}
}

func TestGroupChannelAddParsesChannelMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	parser := command.NewArgParser(groupArgResolver{Client: env.base})

	parsed, err := parser.Parse(
		ctx,
		handlers.GroupArgSpec,
		[]string{"channel", "add", "#**wi-channel**", "WI"},
	)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	args, ok := parsed.(handlers.GroupChannelAddArgs)
	if !ok {
		t.Fatalf("expected GroupChannelAddArgs, got %T", parsed)
	}
	if args.Channel.ChannelID != channelID || args.Channel.Name != "wi-channel" {
		t.Fatalf("unexpected channel: %+v", args.Channel)
	}
}

func TestGroupChannelAddRejectsNumericChannelID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	parser := command.NewArgParser(groupArgResolver{Client: env.base})

	_, err := parser.Parse(
		ctx,
		handlers.GroupArgSpec,
		[]string{"channel", "add", itoa(channelID), "WI"},
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Zulip channel mention") {
		t.Fatalf("expected mention-only error, got %q", userErr.Message)
	}
}

func TestGroupFolderAddAssignsExistingChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupFolderAddArgs{ShortName: "WI"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}

	group, _, err := env.client.GetChannelGroup(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup: %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after group folder add")
	}
	channel, _, err := env.base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil ||
		*channel.Channel.FolderID != *group.ChannelGroup.ChannelFolderID {
		t.Fatalf(
			"channel folder ID = %v, want %d",
			channel.Channel.FolderID,
			*group.ChannelGroup.ChannelFolderID,
		)
	}
}

func TestGroupFolderUnassignUserErrorForChannelInDifferentFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	if _, _, err := env.client.UpdateChannelGroupFolder(ctx, groupID).Add().Execute(); err != nil {
		t.Fatalf("pre-add folder to group %d: %v", groupID, err)
	}

	otherFolder, _, err := env.base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder: %v", err)
	}
	if _, _, err = env.base.UpdateChannel(ctx, channelID).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("move channel to other folder: %v", err)
	}

	h := env.handler(allowAll{})
	_, err = h.Handle(ctx, makeGroupRequest(handlers.GroupFolderUnassignArgs{ShortName: "WI"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "another channel folder") {
		t.Fatalf("expected folder conflict message, got %q", userErr.Message)
	}
	channel, _, err := env.base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil || *channel.Channel.FolderID != otherFolder.ChannelFolderID {
		t.Fatalf(
			"channel folder ID = %v, want %d",
			channel.Channel.FolderID,
			otherFolder.ChannelFolderID,
		)
	}
}

func TestGroupFolderRemoveUserErrorForChannelOutsideGroupInFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	extraChannelID := seedChannel(t, env.base, "manual-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	if _, _, err := env.client.UpdateChannelGroupFolder(ctx, groupID).Add().Execute(); err != nil {
		t.Fatalf("pre-add folder to group %d: %v", groupID, err)
	}
	group, _, err := env.client.GetChannelGroup(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup: %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after group folder add")
	}
	if _, _, err = env.base.UpdateChannel(ctx, extraChannelID).
		FolderID(*group.ChannelGroup.ChannelFolderID).
		Execute(); err != nil {
		t.Fatalf("move extra channel to group folder: %v", err)
	}

	h := env.handler(allowAll{})
	_, err = h.Handle(ctx, makeGroupRequest(handlers.GroupFolderRemoveArgs{ShortName: "WI"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "not part of **WI**") {
		t.Fatalf("expected external channel message, got %q", userErr.Message)
	}
}

func TestGroupFolderRemoveBadRequestIsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	if _, _, err := env.client.UpdateChannelGroupFolder(ctx, groupID).Add().Execute(); err != nil {
		t.Fatalf("pre-add folder to group %d: %v", groupID, err)
	}
	env.base.FailNext(zulipmock.OperationUpdateChannelFolder, z.CodedError{
		Response: z.Response{
			Result: z.ResponseError,
			Msg:    "You need to remove all the channels from this folder to archive it.",
		},
		Code: "BAD_REQUEST",
	})

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupFolderRemoveArgs{ShortName: "WI"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "still contains channels outside **WI**") {
		t.Fatalf("expected archive bad request message, got %q", userErr.Message)
	}
}

func TestGroupFolderAddUserErrorForChannelInDifferentFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "wi-channel")
	if _, _, err := env.client.UpdateChannelGroupChannels(ctx, groupID).Add([]int64{channelID}).Execute(); err != nil {
		t.Fatalf("pre-add channel %d to group %d: %v", channelID, groupID, err)
	}
	otherFolder, _, err := env.base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder: %v", err)
	}
	if _, _, err = env.base.UpdateChannel(ctx, channelID).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("move channel to other folder: %v", err)
	}

	h := env.handler(allowAll{})
	_, err = h.Handle(ctx, makeGroupRequest(handlers.GroupFolderAddArgs{ShortName: "WI"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "another channel folder") {
		t.Fatalf("expected folder conflict message, got %q", userErr.Message)
	}
}

func TestGroupFolderSubcommandParses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	parser := command.NewArgParser(nil)

	parsed, err := parser.Parse(ctx, handlers.GroupArgSpec, []string{"folder", "unassign", "WI"})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	args, ok := parsed.(handlers.GroupFolderUnassignArgs)
	if !ok {
		t.Fatalf("expected GroupFolderUnassignArgs, got %T", parsed)
	}
	if args.ShortName != "WI" {
		t.Fatalf("ShortName = %q, want WI", args.ShortName)
	}
}

func TestGroupChannelCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	h := env.handler(allowAll{})

	result, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "WI"},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "new-channel") {
		t.Errorf("expected result to mention channel name, got: %s", result.Content)
	}
	resp, _, err := env.client.GetChannelGroupChannels(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupChannels: %v", err)
	}
	if len(resp.ChannelIDs) != 1 {
		t.Errorf("expected 1 channel in group %d, got %d", groupID, len(resp.ChannelIDs))
	}
}

func TestGroupChannelCreateReusesExistingChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "existing-channel")
	h := env.handler(allowAll{})

	result, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupChannelCreateArgs{ChannelName: "existing-channel", ShortName: "WI"},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "existing-channel") {
		t.Errorf("expected result to mention channel name, got: %s", result.Content)
	}
	resp, _, err := env.client.GetChannelGroupChannels(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupChannels: %v", err)
	}
	if len(resp.ChannelIDs) != 1 || resp.ChannelIDs[0] != channelID {
		t.Fatalf("group channels = %+v, want [%d]", resp.ChannelIDs, channelID)
	}
}

func TestGroupChannelCreateUnarchivesExistingChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	channelID := seedChannel(t, env.base, "archived-channel")
	if _, _, err := env.base.ArchiveChannel(ctx, channelID).Execute(); err != nil {
		t.Fatalf("ArchiveChannel: %v", err)
	}
	env.base.FailNext(zulipmock.OperationCreateChannel, z.NewAPIError(
		[]byte(
			`{"result":"error","msg":"Channel 'archived-channel' already exists","channel_name":"archived-channel","code":"CHANNEL_ALREADY_EXISTS"}`,
		),
		errors.New("Conflict"),
	))
	h := env.handler(allowAll{})

	_, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupChannelCreateArgs{ChannelName: "archived-channel", ShortName: "WI"},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	resp, _, err := env.client.GetChannelGroupChannels(ctx, groupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupChannels: %v", err)
	}
	if len(resp.ChannelIDs) != 1 || resp.ChannelIDs[0] != channelID {
		t.Fatalf("group channels = %+v, want [%d]", resp.ChannelIDs, channelID)
	}
	channel, _, err := env.base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.IsArchived {
		t.Fatal("channel is still archived")
	}
}

func TestGroupChannelCreateUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	h := env.handler(allowAll{})

	_, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "UNKNOWN"},
		),
	)
	if err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestGroupChannelCreatePermissionDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	h := env.handler(denyAll{})

	_, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "WI"},
		),
	)
	if err == nil {
		t.Fatal("expected error for non-admin user")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
