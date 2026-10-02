package handlers_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	z "github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

func makeChannelRequest(parsedArgs any) command.Request {
	return command.Request{
		ParsedArgs: parsedArgs,
		Actor:      command.Actor{UserID: 123},
		MessageID:  1,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{123}},
	}
}

func TestChannelLsDefaultsToAllChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	seedChannel(t, base, "Algorithms")
	seedChannel(t, base, "Databases")
	h := handlers.NewChannelHandler(client, nil)

	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelLsArgs{}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "Channels matching `.*`:") {
		t.Fatalf("result = %q, want default pattern header", result.Content)
	}
	for _, want := range []string{"#**Algorithms**", "#**Databases**"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("result = %q, want %q", result.Content, want)
		}
	}
}

func TestChannelLsMatchesRegexPattern(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	seedChannel(t, base, "IN0001")
	seedChannel(t, base, "IN0002")
	seedChannel(t, base, "MA0901")
	h := handlers.NewChannelHandler(client, nil)

	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelLsArgs{Pattern: "^IN000[12]$"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	for _, want := range []string{"#**IN0001**", "#**IN0002**"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("result = %q, want %q", result.Content, want)
		}
	}
	for _, unwanted := range []string{"#**MA0901**"} {
		if strings.Contains(result.Content, unwanted) {
			t.Errorf("result = %q, did not expect MA0901 mention %q", result.Content, unwanted)
		}
	}
}

func TestChannelLsRejectsInvalidPattern(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, _ := newChannelGroupClient(t)
	h := handlers.NewChannelHandler(client, nil)

	_, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelLsArgs{Pattern: "IN000["}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("Handle() error = %v, want command.UserError", err)
	}
	if !strings.Contains(userErr.Message, "Invalid channel pattern") {
		t.Fatalf("user error = %q, want invalid pattern message", userErr.Message)
	}
}

func TestChannelFolderAddAssignsFolderWithoutChannelGroupOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	folder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder: %v", err)
	}

	base.FailNext(zulipmock.OperationGetUserGroups, errors.New("must not list user groups"))
	base.FailNext(zulipmock.OperationGetUserGroupMembers, errors.New("must not read user group members"))
	base.FailNext(zulipmock.OperationUpdateUserGroupMembers, errors.New("must not update user group members"))
	base.FailNext(zulipmock.OperationSubscribe, errors.New("must not subscribe channel users"))
	base.FailNext(zulipmock.OperationUnsubscribe, errors.New("must not unsubscribe channel users"))

	h := handlers.NewChannelHandler(client, nil)
	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderAddArgs{
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "manual folder",
	}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil || *channel.Channel.FolderID != folder.ChannelFolderID {
		t.Fatalf(
			"channel folder ID = %v, want %d",
			channel.Channel.FolderID,
			folder.ChannelFolderID,
		)
	}
}

func TestChannelFolderRemoveClearsFolderWithoutChannelGroupOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	folder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder: %v", err)
	}
	if _, _, err := base.UpdateChannel(ctx, channelID).FolderID(folder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("pre-assign channel folder: %v", err)
	}

	base.FailNext(zulipmock.OperationGetUserGroups, errors.New("must not list user groups"))
	base.FailNext(zulipmock.OperationGetUserGroupMembers, errors.New("must not read user group members"))
	base.FailNext(zulipmock.OperationUpdateUserGroupMembers, errors.New("must not update user group members"))
	base.FailNext(zulipmock.OperationSubscribe, errors.New("must not subscribe channel users"))
	base.FailNext(zulipmock.OperationUnsubscribe, errors.New("must not unsubscribe channel users"))

	h := handlers.NewChannelHandler(client, nil)
	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderRemoveArgs{
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "manual folder",
	}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID != nil {
		t.Fatalf("channel folder ID = %d, want nil", *channel.Channel.FolderID)
	}
}

func TestChannelFolderRemoveRejectsDifferentFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	folder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(manual folder): %v", err)
	}
	otherFolder, _, err := base.CreateChannelFolder(ctx).Name("other folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(other folder): %v", err)
	}
	if _, _, err := base.UpdateChannel(ctx, channelID).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("pre-assign channel folder: %v", err)
	}

	h := handlers.NewChannelHandler(client, nil)
	_, err = h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderRemoveArgs{
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "manual folder",
	}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "another channel folder") {
		t.Fatalf("unexpected user error: %q", userErr.Message)
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil || *channel.Channel.FolderID != otherFolder.ChannelFolderID {
		t.Fatalf("channel folder ID = %v, want %d", channel.Channel.FolderID, otherFolder.ChannelFolderID)
	}
	if folder.ChannelFolderID == otherFolder.ChannelFolderID {
		t.Fatal("test setup folders unexpectedly have the same ID")
	}
}

func TestChannelFolderAddRejectsDifferentFolderWithoutForce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	folder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(manual folder): %v", err)
	}
	otherFolder, _, err := base.CreateChannelFolder(ctx).Name("other folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(other folder): %v", err)
	}
	if _, _, err := base.UpdateChannel(ctx, channelID).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("pre-assign channel folder: %v", err)
	}

	h := handlers.NewChannelHandler(client, nil)
	_, err = h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderAddArgs{
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "manual folder",
	}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	for _, want := range []string{"already in another channel folder", "channel folder add -f"} {
		if !strings.Contains(userErr.Message, want) {
			t.Fatalf("user error = %q, want %q", userErr.Message, want)
		}
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil || *channel.Channel.FolderID != otherFolder.ChannelFolderID {
		t.Fatalf("channel folder ID = %v, want %d", channel.Channel.FolderID, otherFolder.ChannelFolderID)
	}
	if folder.ChannelFolderID == otherFolder.ChannelFolderID {
		t.Fatal("test setup folders unexpectedly have the same ID")
	}
}

func TestChannelFolderAddForceReassignsDifferentFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	folder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(manual folder): %v", err)
	}
	otherFolder, _, err := base.CreateChannelFolder(ctx).Name("other folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder(other folder): %v", err)
	}
	if _, _, err := base.UpdateChannel(ctx, channelID).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("pre-assign channel folder: %v", err)
	}

	h := handlers.NewChannelHandler(client, nil)
	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderAddArgs{
		Force:      true,
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "manual folder",
	}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Error("expected non-empty result content")
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if channel.Channel.FolderID == nil || *channel.Channel.FolderID != folder.ChannelFolderID {
		t.Fatalf("channel folder ID = %v, want %d", channel.Channel.FolderID, folder.ChannelFolderID)
	}
}

func TestChannelFolderAddUnknownFolderIsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	if _, _, err := base.CreateChannelFolder(ctx).Name("known folder").Execute(); err != nil {
		t.Fatalf("CreateChannelFolder: %v", err)
	}
	h := handlers.NewChannelHandler(client, nil)

	_, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelFolderAddArgs{
		Channel:    z.Channel{ChannelID: channelID},
		FolderName: "missing folder",
	}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, `Unknown channel folder "missing folder"`) {
		t.Fatalf("unexpected user error: %q", userErr.Message)
	}
	if !strings.Contains(userErr.Message, `"known folder"`) {
		t.Fatalf("expected available folder name in user error, got %q", userErr.Message)
	}
}

func TestChannelFolderSubcommandParses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	parser := command.NewArgParser(groupArgResolver{Client: base})

	parsed, err := parser.Parse(ctx, handlers.ChannelArgSpec, []string{
		"folder", "add", "#**wi-channel**", "manual folder",
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	args, ok := parsed.(handlers.ChannelFolderAddArgs)
	if !ok {
		t.Fatalf("expected ChannelFolderAddArgs, got %T", parsed)
	}
	if args.Channel.ChannelID != channelID || args.Channel.Name != "wi-channel" {
		t.Fatalf("unexpected channel: %+v", args.Channel)
	}
	if args.FolderName != "manual folder" {
		t.Fatalf("FolderName = %q, want manual folder", args.FolderName)
	}
}

func TestChannelFolderAddForceFlagParses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	parser := command.NewArgParser(groupArgResolver{Client: base})

	parsed, err := parser.Parse(ctx, handlers.ChannelArgSpec, []string{
		"folder", "add", "-f", "#**wi-channel**", "manual folder",
	})
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	args, ok := parsed.(handlers.ChannelFolderAddArgs)
	if !ok {
		t.Fatalf("expected ChannelFolderAddArgs, got %T", parsed)
	}
	if !args.Force {
		t.Fatal("Force = false, want true")
	}
	if args.Channel.ChannelID != channelID || args.Channel.Name != "wi-channel" {
		t.Fatalf("unexpected channel: %+v", args.Channel)
	}
	if args.FolderName != "manual folder" {
		t.Fatalf("FolderName = %q, want manual folder", args.FolderName)
	}
}

func TestChannelCreateCreatesChannelWithBotSubscribed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	h := handlers.NewChannelHandler(client, nil)

	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelCreateArgs{ChannelName: "IN0001"}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	idResp, _, err := base.GetChannelID(ctx).Channel("IN0001").Execute()
	if err != nil {
		t.Fatalf("GetChannelID: %v", err)
	}
	if want := "Created channel #**IN0001**."; result.Content != want {
		t.Fatalf("result = %q, want %q", result.Content, want)
	}
	subs, _, err := base.GetSubscribers(ctx, idResp.ChannelID).Execute()
	if err != nil {
		t.Fatalf("GetSubscribers: %v", err)
	}
	if !slices.Equal(subs.Subscribers, []int64{77}) {
		t.Fatalf("subscribers = %v, want [77]", subs.Subscribers)
	}
}

func TestChannelCreateDuplicateIsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	seedChannel(t, base, "IN0001")
	h := handlers.NewChannelHandler(client, nil)

	_, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelCreateArgs{ChannelName: "IN0001"}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("Handle() error = %v, want command.UserError", err)
	}
	if !strings.Contains(userErr.Message, `"IN0001" already exists`) {
		t.Fatalf("user error = %q, want duplicate message", userErr.Message)
	}
}

func TestChannelArchiveArchivesChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedSubscribedChannel(t, base, "IN0001", 77, 101)
	h := handlers.NewChannelHandler(client, nil)

	result, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelArchiveArgs{
		Channel: z.Channel{ChannelID: channelID},
	}))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if want := "Archived channel #**IN0001**."; result.Content != want {
		t.Fatalf("result = %q, want %q", result.Content, want)
	}
	channel, _, err := base.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID: %v", err)
	}
	if !channel.Channel.IsArchived {
		t.Fatal("channel is not archived")
	}
	assertNoSubscribers(t, base, channelID)
}

func TestChannelArchiveAlreadyArchivedIsUserError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "IN0001")
	if _, _, err := base.ArchiveChannel(ctx, channelID).Execute(); err != nil {
		t.Fatalf("ArchiveChannel: %v", err)
	}
	base.FailNext(zulipmock.OperationArchiveChannel, errors.New("must not archive twice"))
	h := handlers.NewChannelHandler(client, nil)

	_, err := h.Handle(ctx, makeChannelRequest(handlers.ChannelArchiveArgs{
		Channel: z.Channel{ChannelID: channelID},
	}))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("Handle() error = %v, want command.UserError", err)
	}
	if !strings.Contains(userErr.Message, "already archived") {
		t.Fatalf("user error = %q, want already archived message", userErr.Message)
	}
}

func TestChannelCreateAndArchiveCommandsParse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, base := newChannelGroupClient(t)
	channelID := seedChannel(t, base, "wi-channel")
	parser := command.NewArgParser(groupArgResolver{Client: base})
	parse := func(content string) (any, error) {
		inv, err := command.Parse(content)
		if err != nil {
			return nil, err
		}
		return parser.Parse(ctx, handlers.ChannelArgSpec, inv.Args)
	}

	parsed, err := parse(`channel create "new channel"`)
	if err != nil {
		t.Fatalf("parse quoted create failed: %v", err)
	}
	if args, ok := parsed.(handlers.ChannelCreateArgs); !ok || args.ChannelName != "new channel" {
		t.Fatalf("parse quoted create = %#v, want ChannelCreateArgs{ChannelName: new channel}", parsed)
	}

	if _, err := parse("channel create new channel"); err == nil {
		t.Fatal("parse unquoted multiword create succeeded, want error")
	}

	parsed, err = parse("channel archive #**wi-channel**")
	if err != nil {
		t.Fatalf("parse archive failed: %v", err)
	}
	if args, ok := parsed.(handlers.ChannelArchiveArgs); !ok || args.Channel.ChannelID != channelID {
		t.Fatalf("parse archive = %#v, want ChannelArchiveArgs for channel %d", parsed, channelID)
	}
}
