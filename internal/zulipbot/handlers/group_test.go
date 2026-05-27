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
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

// --- Fakes for non-Zulip dependencies (auth) ---

// setAnnouncementConfig persists the announcement channel/topic config so the
// handler's storage-backed lookups return them. Pass channelID<=0 or topic=""
// to leave that key unset.
func setAnnouncementConfig(t *testing.T, repo *storage.Repository, channelID int64, topic string) {
	t.Helper()
	if channelID > 0 {
		if err := repo.SetConfigValue(context.Background(), storage.ConfigChange{
			Key:   handlers.KeyAnnouncementChannelID,
			Value: strconv.FormatInt(channelID, 10),
		}); err != nil {
			t.Fatalf("SetConfigValue(channel_id): %v", err)
		}
	}
	if topic != "" {
		if err := repo.SetConfigValue(context.Background(), storage.ConfigChange{
			Key:   handlers.KeyAnnouncementTopic,
			Value: topic,
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

func openGroupTestRepo(t *testing.T) *storage.Repository {
	t.Helper()
	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("storage.Open() failed: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func seedGroupMapping(t *testing.T, repo *storage.Repository, shortName, emojiName string, channelGroupID int64) {
	t.Helper()
	err := repo.UpsertEmojiGroupMapping(context.Background(), storage.EmojiGroupMapping{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}
}

func makeGroupRequest(parsedArgs any) command.Request {
	return command.Request{
		ParsedArgs: parsedArgs,
		Actor:      command.Actor{UserID: 123},
		MessageID:  1,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{123}},
	}
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
func seedChannelGroup(t *testing.T, client channelgroup.Client, base zulipmock.Client, name string) int64 {
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
// in client.GetUserGroups (i.e. `group available`). Returns the new group ID.
func seedZulipUserGroup(t *testing.T, base zulipmock.Client, name, description string, members []int64) int64 {
	t.Helper()
	resp, _, err := base.CreateUserGroup(context.Background()).
		Name(name).
		Description(description).
		Members(members).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup(%q): %v", name, err)
	}
	return resp.GroupID
}

type groupTestEnv struct {
	repo   *storage.Repository
	client channelgroup.Client
	base   zulipmock.Client
}

func newGroupTestEnv(t *testing.T) *groupTestEnv {
	t.Helper()
	client, base := newChannelGroupClient(t)
	return &groupTestEnv{
		repo:   openGroupTestRepo(t),
		client: client,
		base:   base,
	}
}

func (e *groupTestEnv) handler(auth command.Authorizer) *handlers.GroupHandler {
	return handlers.NewGroupHandler(e.client, e.repo, auth, nil)
}

// announcementHash returns the rendered-content hash currently stored, or "" if
// no announcement state exists yet. Tests use this as a proxy for "did the
// handler run an announcement send/edit?": the hash is only saved by
// ensureAnnouncement after a successful SendMessage/UpdateMessage call.
func announcementHash(t *testing.T, repo *storage.Repository) string {
	t.Helper()
	state, ok, err := repo.GetAnnouncementState(context.Background())
	if err != nil {
		t.Fatalf("GetAnnouncementState: %v", err)
	}
	if !ok {
		return ""
	}
	return state.ContentHash
}

// --- Tests ---

func TestGroupSubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)

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

func TestGroupUnsubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)
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

func TestGroupUnsubscribeKeepChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)
	if _, _, err := env.client.SubscribeToChannelGroup(ctx, groupID).
		Principals(z.Principals{UserIDs: &[]int64{123}}).Execute(); err != nil {
		t.Fatalf("pre-subscribe: %v", err)
	}

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupUnsubscribeArgs{KeepChannels: true, ShortName: "WI"}))
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
		t.Errorf("normal user error must not mention 'group mapping list', got: %q", userErr.Message)
	}
	if !strings.Contains(userErr.Message, "help group") && !strings.Contains(userErr.Message, "admin") {
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
		t.Errorf("normal user unsubscribe error must not mention 'group mapping list', got: %q", userErr.Message)
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
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupCreateArgs{ShortName: "PGDP", EmojiName: "books"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	mapping, ok, err := env.repo.GetEmojiGroupMappingByShortName(ctx, "PGDP")
	if err != nil || !ok {
		t.Fatalf("expected created mapping, ok=%v err=%v", ok, err)
	}
	if mapping.ChannelGroupID <= 0 || mapping.EmojiName != "books" {
		t.Fatalf("unexpected mapping: %+v", mapping)
	}
	// The created channel group should exist locally.
	if _, _, err := env.client.GetChannelGroup(ctx, mapping.ChannelGroupID).Execute(); err != nil {
		t.Errorf("expected channel group %d to exist after create, got %v", mapping.ChannelGroupID, err)
	}
	if !strings.Contains(result.Content, "PGDP") || !strings.Contains(result.Content, "books") {
		t.Errorf("expected created group response with name and emoji, got: %s", result.Content)
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
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupCreateArgs{ShortName: "pgdp2", EmojiName: "books"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for duplicate group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "already exists") || !strings.Contains(userErr.Message, "group available") {
		t.Fatalf("expected duplicate group guidance, got: %q", userErr.Message)
	}
}

func TestGroupCreateDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(denyAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupCreateArgs{ShortName: "PGDP", EmojiName: "books"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for denied create, got %T: %v", err, err)
	}
	if _, ok, _ := env.repo.GetEmojiGroupMappingByShortName(ctx, "PGDP"); ok {
		t.Fatal("expected no mapping created when permission denied")
	}
}

func TestGroupAvailableAdminListsZulipVisibleGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	pgdpID := seedZulipUserGroup(t, env.base, "PGDP", "Course PGDP",
		[]int64{
			1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
			21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42,
		})
	wiID := seedZulipUserGroup(t, env.base, "WI", "", []int64{1, 2, 3, 4, 5, 6, 7})

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAvailableArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	pgdpLine := "id=" + itoa(pgdpID)
	wiLine := "id=" + itoa(wiID)
	if !strings.Contains(result.Content, pgdpLine) || !strings.Contains(result.Content, "PGDP") {
		t.Errorf("expected output to mention %s/PGDP, got: %s", pgdpLine, result.Content)
	}
	if !strings.Contains(result.Content, wiLine) || !strings.Contains(result.Content, "WI") {
		t.Errorf("expected output to mention %s/WI, got: %s", wiLine, result.Content)
	}
	if !strings.Contains(result.Content, "42 members") || !strings.Contains(result.Content, "7 members") {
		t.Errorf("expected member counts in output, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "Course PGDP") {
		t.Errorf("expected description in output, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "[imported]") || strings.Contains(result.Content, "[not imported]") {
		t.Errorf("expected no imported/not-imported annotation, got: %s", result.Content)
	}
}

func TestGroupAvailableAdminEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAvailableArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Content), "no zulip channel groups") {
		t.Errorf("expected empty-state message, got: %s", result.Content)
	}
}

func TestGroupAvailableDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	seedZulipUserGroup(t, env.base, "X", "", []int64{1})

	h := env.handler(denyAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAvailableArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for denied access, got %T: %v", err, err)
	}
}

func TestGroupMappingSetAutoImportsWhenZulipVisibleButNotLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedZulipUserGroup(t, env.base, "PGDP", "", []int64{1})
	msgID := int64(555)
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{ShortName: "PGDP", ZulipGroupID: groupID, EmojiName: "math"}),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf("success message should mention auto-import, got: %q", result.Content)
	}
	// Local channel group now exists.
	if _, _, err := env.client.GetChannelGroup(ctx, groupID).Execute(); err != nil {
		t.Errorf("expected channel group %d to exist locally after auto-import, got %v", groupID, err)
	}
	m, ok, err := env.repo.GetEmojiGroupMappingByShortName(ctx, "PGDP")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf("expected mapping stored with channel group %d, got m=%+v ok=%v err=%v", groupID, m, ok, err)
	}
	if announcementHash(t, env.repo) == "" {
		t.Error("expected announcement message to be updated after successful mapping set")
	}
}

func TestGroupMappingSetSkipsAutoImportWhenAlreadyLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "NEWCOURSE")
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	result, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupMappingSetArgs{ShortName: "NEWCOURSE", ZulipGroupID: groupID, EmojiName: "newemoji"},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf("success message must not mention import when no import happened, got: %q", result.Content)
	}
	m, ok, err := env.repo.GetEmojiGroupMappingByShortName(ctx, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf("expected mapping stored with channel group %d, got m=%+v ok=%v err=%v", groupID, m, ok, err)
	}
}

func TestGroupMappingSetRejectsWhenZulipDoesNotKnowGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	seedZulipUserGroup(t, env.base, "OtherGroup", "", []int64{1})
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	// Use an ID that does not match any seeded group.
	bogusID := int64(999999)
	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupMappingSetArgs{ShortName: "PGDP", ZulipGroupID: bogusID, EmojiName: "math"}),
	)
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for invisible Zulip group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "not visible in Zulip") {
		t.Errorf("error should say group is not visible in Zulip, got: %q", userErr.Message)
	}
	if !strings.Contains(userErr.Message, "group available") {
		t.Errorf("error should hint admins to run `group available`, got: %q", userErr.Message)
	}
	if _, ok, err := env.repo.GetEmojiGroupMappingByShortName(ctx, "PGDP"); err != nil || ok {
		t.Errorf("expected mapping not to be stored, got err=%v, ok=%v", err, ok)
	}
	if env.base.LastSentMessage() != nil {
		t.Errorf("expected no message sent on rejected mapping, got %+v", env.base.LastSentMessage())
	}
}

func TestGroupMappingSetAcceptsExistingChannelGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "NEWCOURSE")
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	_, err := h.Handle(
		ctx,
		makeGroupRequest(
			handlers.GroupMappingSetArgs{ShortName: "NEWCOURSE", ZulipGroupID: groupID, EmojiName: "newemoji"},
		),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	m, ok, err := env.repo.GetEmojiGroupMappingByShortName(ctx, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != groupID {
		t.Fatalf("expected mapping stored with channel group %d, got m=%+v ok=%v err=%v", groupID, m, ok, err)
	}
}

func TestGroupMappingListAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)

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
	seedGroupMapping(t, env.repo, "PGDP", "pgdp", 9999)
	seedGroupMapping(t, env.repo, "WI", "wi", wiID)

	msgID := int64(555)
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	_, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError when an enabled mapping references a missing channel group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "PGDP") || !strings.Contains(userErr.Message, "9999") {
		t.Errorf("error should list invalid mapping PGDP/9999, got: %q", userErr.Message)
	}
	if got := announcementHash(t, env.repo); got != "" {
		t.Errorf("expected no announcement update when validation fails, got hash %q", got)
	}
}

func TestGroupAnnounceIgnoresDisabledInvalidMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	wiID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "PGDP", "pgdp", 9999)
	if err := env.repo.SetEmojiGroupMappingEnabled(ctx, "PGDP", false); err != nil {
		t.Fatalf("disable mapping: %v", err)
	}
	seedGroupMapping(t, env.repo, "WI", "wi", wiID)
	msgID := int64(555)
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	if _, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{})); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcementHash(t, env.repo) == "" {
		t.Error("expected announcement message to be updated when only disabled mapping is invalid")
	}
}

func TestGroupAnnounceAllValidMappings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	groupID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)
	msgID := int64(555)
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.repo, 1, "t")

	h := env.handler(allowAll{})
	if _, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceArgs{})); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcementHash(t, env.repo) == "" {
		t.Error("expected announcement message to be updated for valid mappings")
	}
}

func TestGroupMappingListAnnotatesMissingChannelGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newGroupTestEnv(t)
	wiID := seedChannelGroup(t, env.client, env.base, "WI")
	seedGroupMapping(t, env.repo, "PGDP", "pgdp", 9999)
	seedGroupMapping(t, env.repo, "WI", "wi", wiID)

	h := env.handler(allowAll{})
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupMappingListArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "PGDP") || !strings.Contains(result.Content, "missing channel group") {
		t.Errorf("expected PGDP to be flagged as missing, got:\n%s", result.Content)
	}
	if strings.Contains(strings.SplitN(result.Content, "WI", 2)[1], "missing channel group") {
		t.Errorf("expected WI not to be flagged as missing, got:\n%s", result.Content)
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
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
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
	if announcementHash(t, env.repo) == "" {
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
	result, err := h.Handle(ctx, makeGroupRequest(handlers.GroupAnnounceSetMessageArgs{MessageID: 12345}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
	state, ok, err := env.repo.GetAnnouncementState(ctx)
	if err != nil || !ok || state.MessageID == nil || *state.MessageID != 12345 {
		t.Errorf("expected message_id 12345, got state=%+v ok=%v err=%v", state, ok, err)
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
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)
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
	if !strings.Contains(meta.AdminUsage, "group available") {
		t.Errorf("AdminUsage should mention 'group available', got: %q", meta.AdminUsage)
	}
	if strings.Contains(meta.AdminUsage, "group list") {
		t.Errorf("AdminUsage must NOT mention removed 'group list' command, got: %q", meta.AdminUsage)
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
		{"available", handlers.GroupAvailableArgs{}},
		{"mapping_list", handlers.GroupMappingListArgs{}},
		{"announce", handlers.GroupAnnounceArgs{}},
		{"announce_inspect", handlers.GroupAnnounceInspectArgs{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)

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
	if err := env.repo.SaveAnnouncementState(ctx, storage.AnnouncementState{MessageID: &msgID}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	setAnnouncementConfig(t, env.repo, 42, "mytopic")

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
	seedGroupMapping(t, env.repo, "WI", "wi", groupID)
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

func TestGroupChannelCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, groupID := newCourseTestEnv(t)
	h := env.handler(allowAll{})

	result, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "WI"}),
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

func TestGroupChannelCreateUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env, _ := newCourseTestEnv(t)
	h := env.handler(allowAll{})

	_, err := h.Handle(
		ctx,
		makeGroupRequest(handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "UNKNOWN"}),
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
		makeGroupRequest(handlers.GroupChannelCreateArgs{ChannelName: "new-channel", ShortName: "WI"}),
	)
	if err == nil {
		t.Fatal("expected error for non-admin user")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
