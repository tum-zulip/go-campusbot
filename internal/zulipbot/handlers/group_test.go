package handlers_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	z "github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

// --- Fakes ---

type fakeGroupSubscriber struct {
	subscribed   []int64 // channelGroupIDs subscribed
	unsubscribed []int64
	keepChannels []int64
	subErr       error
	unsubErr     error
}

func (s *fakeGroupSubscriber) SubscribeUser(_ context.Context, _ int64, channelGroupID int64) error {
	if s.subErr != nil {
		return s.subErr
	}
	s.subscribed = append(s.subscribed, channelGroupID)
	return nil
}

func (s *fakeGroupSubscriber) UnsubscribeUser(_ context.Context, _ int64, channelGroupID int64) error {
	if s.unsubErr != nil {
		return s.unsubErr
	}
	s.unsubscribed = append(s.unsubscribed, channelGroupID)
	return nil
}

func (s *fakeGroupSubscriber) UnsubscribeUserKeepChannels(_ context.Context, _ int64, channelGroupID int64) error {
	s.keepChannels = append(s.keepChannels, channelGroupID)
	return nil
}

type fakeChannelGroupChecker struct {
	// existing is the set of channel group IDs that exist; if nil, anyExists controls fallback.
	existing  map[int64]bool
	anyExists bool
	err       error
	calls     []int64
	// zulipGroups is the list returned by ListZulipUserGroups. If nil, an
	// empty slice is returned.
	zulipGroups   []channelgroup.ZulipUserGroupSummary
	zulipErr      error
	zulipListCall int
	// imported records every ImportZulipUserGroup call. importErr forces a
	// failure path. importSucceedsLocally controls whether the importer also
	// flips the "existing" map so subsequent ChannelGroupExists calls see the
	// group — matching real behaviour.
	imported              []int64
	importErr             error
	importSucceedsLocally bool
	created               []string
	createID              int64
	createErr             error
	deleted               []int64
	deleteErr             error
}

func (c *fakeChannelGroupChecker) ChannelGroupExists(_ context.Context, channelGroupID int64) (bool, error) {
	c.calls = append(c.calls, channelGroupID)
	if c.err != nil {
		return false, c.err
	}
	if c.existing != nil {
		return c.existing[channelGroupID], nil
	}
	return c.anyExists, nil
}

func (c *fakeChannelGroupChecker) ListZulipUserGroups(_ context.Context) ([]channelgroup.ZulipUserGroupSummary, error) {
	c.zulipListCall++
	if c.zulipErr != nil {
		return nil, c.zulipErr
	}
	return c.zulipGroups, nil
}

func (c *fakeChannelGroupChecker) ImportZulipUserGroup(_ context.Context, userGroupID int64) error {
	if c.importErr != nil {
		return c.importErr
	}
	c.imported = append(c.imported, userGroupID)
	if c.importSucceedsLocally {
		if c.existing == nil {
			c.existing = map[int64]bool{}
		}
		c.existing[userGroupID] = true
	}
	return nil
}

func (c *fakeChannelGroupChecker) CreateChannelGroup(_ context.Context, name string, _ bool) (int64, error) {
	if c.createErr != nil {
		return 0, c.createErr
	}
	c.created = append(c.created, name)
	if c.createID != 0 {
		return c.createID, nil
	}
	return 1, nil
}

func (c *fakeChannelGroupChecker) DeleteChannelGroup(_ context.Context, channelGroupID int64) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.deleted = append(c.deleted, channelGroupID)
	return nil
}

// allExist returns a checker that says every channel group ID exists.
func allExist() *fakeChannelGroupChecker { return &fakeChannelGroupChecker{anyExists: true} }

type fakeAnnouncer struct {
	called int
}

func (a *fakeAnnouncer) UpdateAfterMappingChange(_ context.Context, _ *announcement.SendParams) error {
	a.called++
	return nil
}

type fakeAnnouncementStateAccessor struct {
	state storage.AnnouncementState
	ok    bool
	err   error
}

func (a *fakeAnnouncementStateAccessor) GetAnnouncementState(
	_ context.Context,
) (storage.AnnouncementState, bool, error) {
	return a.state, a.ok, a.err
}

func (a *fakeAnnouncementStateAccessor) SaveAnnouncementState(
	_ context.Context,
	state storage.AnnouncementState,
) error {
	a.state = state
	a.ok = true
	return nil
}

type fakeGroupConfigReader struct {
	channelID int64
	topic     string
	channelOK bool
	topicOK   bool
}

func (r *fakeGroupConfigReader) AnnouncementChannelID(_ context.Context) (int64, bool, error) {
	return r.channelID, r.channelOK, nil
}

func (r *fakeGroupConfigReader) AnnouncementTopic(_ context.Context) (string, bool, error) {
	return r.topic, r.topicOK, nil
}

type failingMappingWriter struct {
	*storage.Repository

	upsertErr error
}

func (w failingMappingWriter) UpsertEmojiGroupMapping(ctx context.Context, m storage.EmojiGroupMapping) error {
	if w.upsertErr != nil {
		return w.upsertErr
	}
	return w.Repository.UpsertEmojiGroupMapping(ctx, m)
}

type allowAll struct{}

func (allowAll) Check(_ context.Context, _ command.Actor, _ z.Role) error { return nil }

type denyAll struct{}

func (denyAll) Check(_ context.Context, _ command.Actor, _ z.Role) error {
	return command.ErrDenied
}

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

func makeGroupRequest(name string, args ...string) command.Request {
	return command.Request{
		Invocation: command.Invocation{Name: "group", Args: append([]string{name}, args...)},
		Actor:      command.Actor{UserID: 123},
		MessageID:  1,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{123}},
	}
}

func TestGroupSubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	sub := &fakeGroupSubscriber{}
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("subscribe", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	if len(sub.subscribed) != 1 || sub.subscribed[0] != 42 {
		t.Errorf("expected subscribed to group 42, got %v", sub.subscribed)
	}
}

func TestGroupUnsubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	sub := &fakeGroupSubscriber{}
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("unsubscribe", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	if len(sub.unsubscribed) != 1 || sub.unsubscribed[0] != 42 {
		t.Errorf("expected unsubscribed from group 42, got %v", sub.unsubscribed)
	}
}

func TestGroupUnsubscribeKeepChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	sub := &fakeGroupSubscriber{}
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("unsubscribe", "-k", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	if len(sub.keepChannels) != 1 || sub.keepChannels[0] != 42 {
		t.Errorf("expected keepChannels for group 42, got %v", sub.keepChannels)
	}
	if len(sub.unsubscribed) != 0 {
		t.Errorf("expected no full unsubscribe, got %v", sub.unsubscribed)
	}
}

func TestGroupSubscribeUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	sub := &fakeGroupSubscriber{}
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("subscribe", "UNKNOWN"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for unknown group, got %T: %v", err, err)
	}
}

func TestGroupSubscribeUnknownGroupNoneUserDoesNotLeakAdminHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	sub := &fakeGroupSubscriber{}
	// denyAll simulates a non-admin actor (auth.Check returns error for PermAdmin)
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("subscribe", "UNKNOWN"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for unknown group, got %T: %v", err, err)
	}
	if strings.Contains(userErr.Message, "group mapping list") {
		t.Errorf("normal user error must not mention 'group mapping list', got: %q", userErr.Message)
	}
	// Should suggest help or admin
	if !strings.Contains(userErr.Message, "help group") && !strings.Contains(userErr.Message, "admin") {
		t.Errorf("normal user error should suggest help or admin, got: %q", userErr.Message)
	}
}

func TestGroupSubscribeUnknownGroupAdminSeesHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	sub := &fakeGroupSubscriber{}
	// allowAll simulates an admin actor (auth.Check returns nil for PermAdmin)
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("subscribe", "UNKNOWN"))
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
	repo := openGroupTestRepo(t)

	sub := &fakeGroupSubscriber{}
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("unsubscribe", "UNKNOWN"))
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
	repo := openGroupTestRepo(t)

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("list"))
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
	repo := openGroupTestRepo(t)
	checker := &fakeChannelGroupChecker{createID: 77}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("create", "PGDP", "books"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if len(checker.created) != 1 || checker.created[0] != "PGDP" {
		t.Fatalf("expected CreateChannelGroup(PGDP), got %v", checker.created)
	}
	mapping, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "PGDP")
	if err != nil || !ok {
		t.Fatalf("expected created mapping, ok=%v err=%v", ok, err)
	}
	if mapping.ChannelGroupID != 77 || mapping.EmojiName != "books" {
		t.Fatalf("unexpected mapping: %+v", mapping)
	}
	if !strings.Contains(result.Content, "77") || !strings.Contains(result.Content, "PGDP") ||
		!strings.Contains(result.Content, "books") {
		t.Errorf("expected created group response with name and id, got: %s", result.Content)
	}
}

func TestGroupCreateDuplicateZulipUserGroupReturnsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	checker := &fakeChannelGroupChecker{
		createErr: z.CodedError{
			Response: z.Response{Result: z.ResponseError, Msg: "User group 'pgdp2' already exists."},
			Code:     "BAD_REQUEST",
		},
	}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("create", "pgdp2", "books"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for duplicate group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "already exists") || !strings.Contains(userErr.Message, "group available") {
		t.Fatalf("expected duplicate group guidance, got: %q", userErr.Message)
	}
}

func TestGroupCreateRollsBackChannelGroupWhenMappingFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	checker := &fakeChannelGroupChecker{createID: 77}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		failingMappingWriter{Repository: repo, upsertErr: errors.New("mapping write failed")},
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("create", "PGDP", "books"))
	if err == nil {
		t.Fatal("expected create error")
	}
	if len(checker.deleted) != 1 || checker.deleted[0] != 77 {
		t.Fatalf("expected DeleteChannelGroup(77), got %v", checker.deleted)
	}
	_, ok, getErr := repo.GetEmojiGroupMappingByShortName(ctx, "PGDP")
	if getErr != nil {
		t.Fatalf("GetEmojiGroupMappingByShortName() failed: %v", getErr)
	}
	if ok {
		t.Fatal("expected no mapping after rollback")
	}
}

func TestGroupCreateDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	checker := &fakeChannelGroupChecker{}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("create", "PGDP", "books"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for denied create, got %T: %v", err, err)
	}
	if len(checker.created) != 0 {
		t.Fatalf("expected no CreateChannelGroup call, got %v", checker.created)
	}
}

func TestGroupAvailableAdminListsZulipVisibleGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	checker := &fakeChannelGroupChecker{
		zulipGroups: []channelgroup.ZulipUserGroupSummary{
			{ID: 30, Name: "PGDP", Description: "Course PGDP", MemberCount: 42},
			{ID: 42, Name: "WI", Description: "", MemberCount: 7},
		},
	}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("available"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "id=30") || !strings.Contains(result.Content, "PGDP") {
		t.Errorf("expected output to mention id=30/PGDP, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "id=42") || !strings.Contains(result.Content, "WI") {
		t.Errorf("expected output to mention id=42/WI, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "42 members") || !strings.Contains(result.Content, "7 members") {
		t.Errorf("expected member counts in output, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "Course PGDP") {
		t.Errorf("expected description in output, got: %s", result.Content)
	}
	// Imported/not-imported annotations have been removed: admins use the
	// Zulip ID directly and auto-import happens on `group mapping set`.
	if strings.Contains(result.Content, "[imported]") || strings.Contains(result.Content, "[not imported]") {
		t.Errorf("expected no imported/not-imported annotation, got: %s", result.Content)
	}
	if checker.zulipListCall != 1 {
		t.Errorf("expected ListZulipUserGroups called once, got %d", checker.zulipListCall)
	}
}

func TestGroupAvailableAdminEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	checker := &fakeChannelGroupChecker{zulipGroups: nil}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("available"))
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
	repo := openGroupTestRepo(t)

	checker := &fakeChannelGroupChecker{
		zulipGroups: []channelgroup.ZulipUserGroupSummary{{ID: 1, Name: "X"}},
	}
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("available"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for denied access, got %T: %v", err, err)
	}
	if checker.zulipListCall != 0 {
		t.Errorf("denied user must not trigger ListZulipUserGroups, got %d calls", checker.zulipListCall)
	}
}

func TestGroupMappingSetAutoImportsWhenZulipVisibleButNotLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	msgID := int64(555)
	stateAccessor := &fakeAnnouncementStateAccessor{state: storage.AnnouncementState{MessageID: &msgID}, ok: true}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	checker := &fakeChannelGroupChecker{
		existing:              map[int64]bool{}, // not local yet
		zulipGroups:           []channelgroup.ZulipUserGroupSummary{{ID: 30, Name: "PGDP", MemberCount: 1}},
		importSucceedsLocally: true,
	}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("mapping", "set", "PGDP", "30", "math"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf("success message should mention auto-import, got: %q", result.Content)
	}
	if len(checker.imported) != 1 || checker.imported[0] != 30 {
		t.Errorf("expected ImportZulipUserGroup(30) to be called, got %v", checker.imported)
	}
	if checker.zulipListCall == 0 {
		t.Error("expected ListZulipUserGroups to be consulted to verify visibility")
	}
	// Mapping stored.
	m, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "PGDP")
	if err != nil || !ok || m.ChannelGroupID != 30 {
		t.Fatalf("expected mapping stored with channel group 30, got m=%+v ok=%v err=%v", m, ok, err)
	}
	// Announcement triggered.
	if announcer.called == 0 {
		t.Error("expected announcer to be called after successful mapping set")
	}
}

func TestGroupMappingSetSkipsAutoImportWhenAlreadyLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	// Group is already local — no Zulip lookup or import should happen.
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{55: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		&fakeAnnouncementStateAccessor{},
		config,
		allowAll{},
	)

	result, err := h.Handle(
		ctx,
		makeGroupRequest("mapping", "set", "NEWCOURSE", "55", "newemoji"),
	)
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if strings.Contains(strings.ToLower(result.Content), "imported") {
		t.Errorf("success message must not mention import when no import happened, got: %q", result.Content)
	}
	if len(checker.imported) != 0 {
		t.Errorf("expected no ImportZulipUserGroup call, got %v", checker.imported)
	}
	if checker.zulipListCall != 0 {
		t.Errorf("expected no ListZulipUserGroups call when group already local, got %d", checker.zulipListCall)
	}
	m, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != 55 {
		t.Fatalf("expected mapping stored with channel group 55, got m=%+v ok=%v err=%v", m, ok, err)
	}
}

func TestGroupMappingSetRejectsWhenZulipDoesNotKnowGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	// Not local, and Zulip lists a DIFFERENT group — id 30 is unknown.
	checker := &fakeChannelGroupChecker{
		existing:    map[int64]bool{},
		zulipGroups: []channelgroup.ZulipUserGroupSummary{{ID: 99, Name: "OtherGroup"}},
	}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		&fakeAnnouncementStateAccessor{},
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("mapping", "set", "PGDP", "30", "math"))
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
	// No import attempted (visibility check failed first).
	if len(checker.imported) != 0 {
		t.Errorf("expected no import on failure, got %v", checker.imported)
	}
	// Mapping must NOT have been stored.
	if _, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "PGDP"); err != nil || ok {
		t.Errorf("expected mapping not to be stored, got err=%v, ok=%v", err, ok)
	}
	// Announcement must NOT have been triggered.
	if announcer.called != 0 {
		t.Errorf("expected announcer not to be called on rejected mapping, got %d calls", announcer.called)
	}
}

func TestGroupMappingSetFailedImportLeavesNoMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	checker := &fakeChannelGroupChecker{
		existing:    map[int64]bool{},
		zulipGroups: []channelgroup.ZulipUserGroupSummary{{ID: 30, Name: "PGDP"}},
		importErr:   errors.New("disk full"),
	}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		&fakeAnnouncementStateAccessor{},
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("mapping", "set", "PGDP", "30", "math"))
	if err == nil {
		t.Fatal("expected error when ImportZulipUserGroup fails")
	}
	// Mapping must NOT have been stored.
	if _, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "PGDP"); err != nil || ok {
		t.Errorf("expected mapping not to be stored, got err=%v, ok=%v", err, ok)
	}
	// Announcement must NOT have been triggered.
	if announcer.called != 0 {
		t.Errorf("expected announcer not to be called on failed import, got %d calls", announcer.called)
	}
}

func TestGroupMappingSetAcceptsExistingChannelGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	stateAccessor := &fakeAnnouncementStateAccessor{}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{55: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("mapping", "set", "NEWCOURSE", "55", "newemoji"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	m, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "NEWCOURSE")
	if err != nil || !ok || m.ChannelGroupID != 55 {
		t.Fatalf("expected mapping stored with channel group 55, got m=%+v ok=%v err=%v", m, ok, err)
	}
}

func TestGroupMappingListAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("mapping", "list"))
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
	repo := openGroupTestRepo(t)

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("mapping", "list"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for denied access, got %T: %v", err, err)
	}
}

func TestGroupAnnounceNoConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	config := &fakeGroupConfigReader{channelOK: false}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		announcer,
		&fakeAnnouncementStateAccessor{},
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("announce"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError when config not set, got %T: %v", err, err)
	}
}

func TestGroupAnnounceRejectsInvalidEnabledMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	// Pre-seed a bad enabled mapping directly in storage, simulating an existing
	// bad row that predates the validation.
	seedGroupMapping(t, repo, "PGDP", "pgdp", 30)
	// Also seed a valid mapping.
	seedGroupMapping(t, repo, "WI", "wi", 42)

	announcer := &fakeAnnouncer{}
	msgID := int64(555)
	stateAccessor := &fakeAnnouncementStateAccessor{state: storage.AnnouncementState{MessageID: &msgID}, ok: true}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	// Only group 42 exists.
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{42: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("announce"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError when an enabled mapping references a missing channel group, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "PGDP") || !strings.Contains(userErr.Message, "30") {
		t.Errorf("error should list invalid mapping PGDP/30, got: %q", userErr.Message)
	}
	if announcer.called != 0 {
		t.Errorf("expected announcer not to be called when validation fails, got %d", announcer.called)
	}
}

func TestGroupAnnounceIgnoresDisabledInvalidMapping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	// Seed a bad mapping, then disable it.
	seedGroupMapping(t, repo, "PGDP", "pgdp", 30)
	if err := repo.SetEmojiGroupMappingEnabled(ctx, "PGDP", false); err != nil {
		t.Fatalf("disable mapping: %v", err)
	}
	// Seed a valid enabled mapping so announcement has something to render.
	seedGroupMapping(t, repo, "WI", "wi", 42)

	announcer := &fakeAnnouncer{}
	msgID := int64(555)
	stateAccessor := &fakeAnnouncementStateAccessor{state: storage.AnnouncementState{MessageID: &msgID}, ok: true}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{42: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	if _, err := h.Handle(ctx, makeGroupRequest("announce")); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcer.called != 1 {
		t.Errorf("expected announcer to run when only disabled mapping is invalid, got %d calls", announcer.called)
	}
}

func TestGroupAnnounceAllValidMappings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	announcer := &fakeAnnouncer{}
	msgID := int64(555)
	stateAccessor := &fakeAnnouncementStateAccessor{state: storage.AnnouncementState{MessageID: &msgID}, ok: true}
	config := &fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true}
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{42: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	if _, err := h.Handle(ctx, makeGroupRequest("announce")); err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if announcer.called != 1 {
		t.Errorf("expected announcer to be called once for valid mappings, got %d", announcer.called)
	}
}

func TestGroupMappingListAnnotatesMissingChannelGroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "PGDP", "pgdp", 30)
	seedGroupMapping(t, repo, "WI", "wi", 42)
	checker := &fakeChannelGroupChecker{existing: map[int64]bool{42: true}}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		checker,
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("mapping", "list"))
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
	repo := openGroupTestRepo(t)

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("badcmd"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for invalid subcommand, got %T: %v", err, err)
	}
}

func TestGroupAnnounceExistingMessageID(t *testing.T) {
	// If message_id stored in state, announce succeeds without channel/topic
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	msgID := int64(555)
	stateAccessor := &fakeAnnouncementStateAccessor{
		state: storage.AnnouncementState{MessageID: &msgID},
		ok:    true,
	}
	config := &fakeGroupConfigReader{} // no channel/topic configured

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("announce"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
	if announcer.called != 1 {
		t.Errorf("expected announcer called once, got %d", announcer.called)
	}
}

func TestGroupAnnounceNoConfigNoMessageID(t *testing.T) {
	// No message_id, no channel/topic → user error mentioning set-message
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	announcer := &fakeAnnouncer{}
	config := &fakeGroupConfigReader{channelOK: false}
	stateAccessor := &fakeAnnouncementStateAccessor{ok: false} // no stored state

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		announcer,
		stateAccessor,
		config,
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("announce"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError when no config and no message_id, got %T: %v", err, err)
	}
}

func TestGroupAnnounceSetMessageValid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	stateAccessor := &fakeAnnouncementStateAccessor{}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		stateAccessor,
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("announce", "set-message", "12345"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
	if stateAccessor.state.MessageID == nil || *stateAccessor.state.MessageID != 12345 {
		t.Errorf("expected message_id 12345, got %v", stateAccessor.state.MessageID)
	}
}

func TestGroupAnnounceSetMessageInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	// Zero
	_, err := h.Handle(ctx, makeGroupRequest("announce", "set-message", "0"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for 0, got %T: %v", err, err)
	}

	// Negative
	_, err = h.Handle(ctx, makeGroupRequest("announce", "set-message", "-1"))
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for -1, got %T: %v", err, err)
	}

	// Non-numeric
	_, err = h.Handle(ctx, makeGroupRequest("announce", "set-message", "abc"))
	if !errors.As(err, &userErr) {
		t.Errorf("expected UserError for abc, got %T: %v", err, err)
	}
}

// --- Help metadata / visibility tests ---

// buildGroupHandler creates a GroupHandler backed by a real in-memory SQLite repo.
func buildGroupHandler(t *testing.T, auth command.Authorizer) *handlers.GroupHandler {
	t.Helper()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)
	return handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo, repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{channelID: 1, topic: "t", channelOK: true, topicOK: true},
		auth,
	)
}

// buildGroupHelpHandler creates a command.Registry+HelpHandler with only the GroupHandler registered.
func buildGroupHelpHandler(t *testing.T, role z.Role) (*command.Registry, *command.HelpHandler) {
	t.Helper()
	registry := command.NewRegistry()
	gh := buildGroupHandler(t, allowAll{})
	if err := registry.Register(gh); err != nil {
		t.Fatalf("Register(group) failed: %v", err)
	}
	provider := staticGroupRoleProvider{role: role}
	return registry, command.NewHelpHandler(registry, provider)
}

// staticGroupRoleProvider satisfies command.RoleProvider for group help tests.
type staticGroupRoleProvider struct{ role z.Role }

func (p staticGroupRoleProvider) RoleFor(_ context.Context, _ command.Actor) (z.Role, error) {
	return p.role, nil
}

func runGroupHelp(t *testing.T, h *command.HelpHandler, actor command.Actor, args ...string) (string, error) {
	t.Helper()
	result, err := h.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "help", Args: args},
		Actor:      actor,
	})
	return result.Content, err
}

func TestGroupMetadataAdminUsageIsSet(t *testing.T) {
	t.Parallel()
	gh := buildGroupHandler(t, allowAll{})
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

func TestGroupHelpNoneUserSeesSubscribeNotAdminSubcommands(t *testing.T) {
	t.Parallel()
	_, h := buildGroupHelpHandler(t, z.RoleMember)
	out, err := runGroupHelp(t, h, command.Actor{UserID: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "group") {
		t.Errorf("member should see 'group' in help, got: %q", out)
	}
	// subscribe/unsubscribe text comes from public Usage
	if !strings.Contains(out, "subscribe") {
		t.Errorf("member should see 'subscribe' in help, got: %q", out)
	}
	if strings.Contains(out, "mapping") {
		t.Errorf("member must NOT see 'mapping' in help, got: %q", out)
	}
	if strings.Contains(out, "announce") {
		t.Errorf("member must NOT see 'announce' in help, got: %q", out)
	}
	if strings.Contains(out, "group available") {
		t.Errorf("member must NOT see 'group available' in help, got: %q", out)
	}
}

func TestGroupHelpAdminSeesAdminSubcommands(t *testing.T) {
	t.Parallel()
	_, h := buildGroupHelpHandler(t, z.RoleAdmin)
	out, err := runGroupHelp(t, h, command.Actor{UserID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "subscribe") {
		t.Errorf("admin should see 'subscribe', got: %q", out)
	}
	if !strings.Contains(out, "mapping") {
		t.Errorf("admin should see 'mapping', got: %q", out)
	}
	if !strings.Contains(out, "announce") {
		t.Errorf("admin should see 'announce', got: %q", out)
	}
	if strings.Contains(out, "group list") {
		t.Errorf("admin must NOT see removed 'group list' command, got: %q", out)
	}
	if !strings.Contains(out, "group available") {
		t.Errorf("admin should see 'group available', got: %q", out)
	}
}

func TestGroupHelpOwnerSeesAdminSubcommands(t *testing.T) {
	t.Parallel()
	_, h := buildGroupHelpHandler(t, z.RoleOwner)
	out, err := runGroupHelp(t, h, command.Actor{UserID: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "mapping") {
		t.Errorf("owner should see 'mapping', got: %q", out)
	}
	if !strings.Contains(out, "announce") {
		t.Errorf("owner should see 'announce', got: %q", out)
	}
}

func TestGroupHelpNoneUserLookupGroupDoesNotShowAdminSubcommands(t *testing.T) {
	t.Parallel()
	_, h := buildGroupHelpHandler(t, z.RoleMember)
	out, err := runGroupHelp(t, h, command.Actor{UserID: 1}, "group")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "mapping") {
		t.Errorf("member 'help group' must not show 'mapping', got: %q", out)
	}
	if strings.Contains(out, "announce") {
		t.Errorf("member 'help group' must not show 'announce', got: %q", out)
	}
	if strings.Contains(out, "group available") {
		t.Errorf("member 'help group' must not show 'group available', got: %q", out)
	}
}

func TestGroupHelpAdminLookupGroupShowsAdminSubcommands(t *testing.T) {
	t.Parallel()
	_, h := buildGroupHelpHandler(t, z.RoleAdmin)
	out, err := runGroupHelp(t, h, command.Actor{UserID: 2}, "group")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "mapping") {
		t.Errorf("admin 'help group' should show 'mapping', got: %q", out)
	}
	if !strings.Contains(out, "announce") {
		t.Errorf("admin 'help group' should show 'announce', got: %q", out)
	}
	if !strings.Contains(out, "group available") {
		t.Errorf("admin 'help group' should show 'group available', got: %q", out)
	}
}

func TestGroupAdminCommandDeniedForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)

	// denyAll simulates a real auth check that denies non-admin actors.
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo, repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		denyAll{},
	)

	for _, args := range [][]string{
		{"available"},
		{"mapping", "list"},
		{"announce"},
		{"announce", "inspect"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := h.Handle(ctx, makeGroupRequest(args[0], args[1:]...))
			var userErr command.UserError
			if !errors.As(err, &userErr) {
				t.Errorf("expected UserError for denied %v, got %T: %v", args, err, err)
			}
		})
	}
}

func TestGroupSubscribeStillWorksForNoneUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	sub := &fakeGroupSubscriber{}
	// allowAll auth: subscribe doesn't check auth anyway, but use allowAll to be clear
	h := handlers.NewGroupHandler(
		sub,
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("subscribe", "WI"))
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
	repo := openGroupTestRepo(t)

	msgID := int64(999)
	stateAccessor := &fakeAnnouncementStateAccessor{
		state: storage.AnnouncementState{MessageID: &msgID},
		ok:    true,
	}
	config := &fakeGroupConfigReader{channelID: 42, topic: "mytopic", channelOK: true, topicOK: true}

	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo,
		repo,
		&fakeAnnouncer{},
		stateAccessor,
		config,
		allowAll{},
	)

	result, err := h.Handle(ctx, makeGroupRequest("announce", "inspect"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty inspect output")
	}
	// Should mention message_id
	if !strings.Contains(result.Content, "999") {
		t.Errorf("expected inspect output to contain message_id 999, got: %s", result.Content)
	}
}

func newGroupTestChannelGroupClient(t *testing.T) channelgroup.Client {
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
	client, err := channelgroup.NewClient(
		context.Background(),
		zulipmock.NewClient(),
		db,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("channelgroup.NewClient: %v", err)
	}
	return client
}

func buildCourseHandler(
	t *testing.T,
	auth command.Authorizer,
) (*handlers.GroupHandler, channelgroup.Client) {
	t.Helper()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)

	client := newGroupTestChannelGroupClient(t)
	if err := client.ImportZulipUserGroup(context.Background(), 42); err != nil {
		t.Fatalf("ImportZulipUserGroup: %v", err)
	}

	svc := channelgroup.NewGroupService(client)
	h := handlers.NewGroupHandler(
		svc,
		svc,
		repo, repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		auth,
	).WithChannelManager(svc)
	return h, client
}

func TestGroupCourseAdd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, client := buildCourseHandler(t, allowAll{})

	result, err := h.Handle(ctx, makeGroupRequest("course", "add", "99", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := client.GetIsChannelInChannelGroup(ctx, 42, 99).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelInChannelGroup: %v", err)
	}
	if !resp.IsChannelGroupMember {
		t.Error("expected channel 99 to be in group 42 after add")
	}
}

func TestGroupCourseRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, client := buildCourseHandler(t, allowAll{})

	if _, _, err := client.UpdateChannelGroupChannels(ctx, 42).Add([]int64{99}).Execute(); err != nil {
		t.Fatalf("pre-add channel 99 to group 42: %v", err)
	}

	result, err := h.Handle(ctx, makeGroupRequest("course", "remove", "99", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	resp, _, err := client.GetIsChannelInChannelGroup(ctx, 42, 99).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelInChannelGroup: %v", err)
	}
	if resp.IsChannelGroupMember {
		t.Error("expected channel 99 to not be in group 42 after remove")
	}
}

func TestGroupCoursePermissionDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := buildCourseHandler(t, denyAll{})

	_, err := h.Handle(ctx, makeGroupRequest("course", "add", "99", "WI"))
	if err == nil {
		t.Fatal("expected error for non-admin user")
	}
}

func TestGroupCourseUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := buildCourseHandler(t, allowAll{})

	_, err := h.Handle(ctx, makeGroupRequest("course", "add", "99", "UNKNOWN"))
	if err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestGroupCourseInvalidChannelID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := buildCourseHandler(t, allowAll{})

	_, err := h.Handle(ctx, makeGroupRequest("course", "add", "notanint", "WI"))
	if err == nil {
		t.Fatal("expected error for invalid channel_id")
	}
}

func TestGroupCourseNotConfigured(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openGroupTestRepo(t)
	seedGroupMapping(t, repo, "WI", "wi", 42)
	h := handlers.NewGroupHandler(
		&fakeGroupSubscriber{},
		allExist(),
		repo, repo,
		&fakeAnnouncer{},
		&fakeAnnouncementStateAccessor{},
		&fakeGroupConfigReader{},
		allowAll{},
	)

	_, err := h.Handle(ctx, makeGroupRequest("course", "add", "99", "WI"))
	if err == nil {
		t.Fatal("expected error when channel manager is not configured")
	}
}

func TestGroupChannelCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, client := buildCourseHandler(t, allowAll{})

	result, err := h.Handle(ctx, makeGroupRequest("channel", "create", "new-channel", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "new-channel") {
		t.Errorf("expected result to mention channel name, got: %s", result.Content)
	}
	resp, _, err := client.GetChannelGroupChannels(ctx, 42).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupChannels: %v", err)
	}
	if len(resp.ChannelIDs) != 1 {
		t.Errorf("expected 1 channel in group 42, got %d", len(resp.ChannelIDs))
	}
}

func TestGroupChannelCreateCourseAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, client := buildCourseHandler(t, allowAll{})

	result, err := h.Handle(ctx, makeGroupRequest("course", "create", "new-channel", "WI"))
	if err != nil {
		t.Fatalf("Handle() failed via course alias: %v", err)
	}
	if !strings.Contains(result.Content, "new-channel") {
		t.Errorf("expected result to mention channel name, got: %s", result.Content)
	}
	resp, _, err := client.GetChannelGroupChannels(ctx, 42).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupChannels: %v", err)
	}
	if len(resp.ChannelIDs) != 1 {
		t.Errorf("expected 1 channel in group 42, got %d", len(resp.ChannelIDs))
	}
}

func TestGroupChannelCreateUnknownGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := buildCourseHandler(t, allowAll{})

	_, err := h.Handle(ctx, makeGroupRequest("channel", "create", "new-channel", "UNKNOWN"))
	if err == nil {
		t.Fatal("expected error for unknown group")
	}
}

func TestGroupChannelCreatePermissionDenied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, _ := buildCourseHandler(t, denyAll{})

	_, err := h.Handle(ctx, makeGroupRequest("channel", "create", "new-channel", "WI"))
	if err == nil {
		t.Fatal("expected error for non-admin user")
	}
}
