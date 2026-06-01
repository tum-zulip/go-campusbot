package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

type ChannelHandler struct {
	client zulipclient.Client
	logger *slog.Logger
}

type ChannelLsArgs struct {
	Pattern string `arg:"pattern" optional:"true" desc:"regular expression for channel names"`
}

type ChannelFolderAddArgs struct {
	Force      bool          `arg:"-f"          desc:"Reassign the channel if it is already in another folder"`
	Channel    zulip.Channel `arg:"channel"     desc:"Zulip channel mention"                                   mention_only:"true"`
	FolderName string        `arg:"folder_name" desc:"Zulip channel folder name"`
}

type ChannelFolderRemoveArgs struct {
	Channel    zulip.Channel `arg:"channel"     mention_only:"true" desc:"Zulip channel mention"`
	FolderName string        `arg:"folder_name"                     desc:"Zulip channel folder name"`
}

var ChannelArgSpec = command.SubcmdSpec{ //nolint:gochecknoglobals,revive // package-level command spec shared by metadata
	"ls": ChannelLsArgs{},
	"folder": command.SubcmdSpec{
		"add":    ChannelFolderAddArgs{},
		"remove": ChannelFolderRemoveArgs{},
	},
}

func NewChannelHandler(client zulipclient.Client, logger *slog.Logger) *ChannelHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChannelHandler{client: client, logger: logger}
}

func (h *ChannelHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:       "channel",
		Summary:    "List or update Zulip channels.",
		Usage:      "channel ls [pattern]\nchannel folder add [-f] <channel_mention> <folder_name>\nchannel folder remove <channel_mention> <folder_name>",
		Permission: command.PermAdmin,
		ArgSpec:    ChannelArgSpec,
	}
}

func (h *ChannelHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	h.logger.DebugContext(ctx, "handling channel command",
		"parsed_args_type", fmt.Sprintf("%T", req.ParsedArgs),
		"actor_user_id", req.Actor.UserID,
		"message_id", req.MessageID)
	switch args := req.ParsedArgs.(type) {
	case ChannelLsArgs:
		return h.handleLs(ctx, args)
	case ChannelFolderAddArgs:
		return h.handleFolderAdd(ctx, args)
	case ChannelFolderRemoveArgs:
		return h.handleFolderRemove(ctx, args)
	default:
		return command.Result{}, command.NewUserError(
			"Usage: `channel ls [pattern]` or `channel folder <add|remove> <channel_mention> <folder_name>`",
		)
	}
}

func (h *ChannelHandler) handleLs(ctx context.Context, args ChannelLsArgs) (command.Result, error) {
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		pattern = ".*"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Invalid channel pattern %q: %v", pattern, err))
	}

	resp, _, err := h.client.GetChannels(ctx).IncludeAll(true).Execute()
	if err != nil {
		return command.Result{}, fmt.Errorf("list channels: %w", err)
	}
	if resp == nil {
		return command.Result{}, errors.New("list channels: empty response")
	}

	matches := make([]zulip.Channel, 0)
	for _, channel := range resp.Channels {
		if re.MatchString(channel.Name) {
			matches = append(matches, channel)
		}
	}
	if len(matches) == 0 {
		return command.Result{Content: fmt.Sprintf("No channels match `%s`.", pattern)}, nil
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Name == matches[j].Name {
			return matches[i].ChannelID < matches[j].ChannelID
		}
		return matches[i].Name < matches[j].Name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "Channels matching `%s`:\n", pattern)
	for _, channel := range matches {
		fmt.Fprintf(&b, "- %s\n", zulipChannelMention(channel.Name, channel.ChannelID))
	}
	return command.Result{Content: strings.TrimSpace(b.String())}, nil
}

func (h *ChannelHandler) handleFolderAdd(
	ctx context.Context,
	args ChannelFolderAddArgs,
) (command.Result, error) {
	if args.Channel.ChannelID <= 0 {
		return command.Result{}, command.NewUserError("channel_id must be a positive integer")
	}
	folderName := strings.TrimSpace(args.FolderName)
	if folderName == "" {
		return command.Result{}, command.NewUserError("folder_name must not be empty")
	}
	folderID, err := h.channelFolderIDByName(ctx, folderName)
	if err != nil {
		return command.Result{}, err
	}
	channelResp, _, err := h.client.GetChannelByID(ctx, args.Channel.ChannelID).Execute()
	if err != nil {
		return command.Result{}, fmt.Errorf("get channel %d: %w", args.Channel.ChannelID, err)
	}
	if channelResp == nil {
		return command.Result{}, fmt.Errorf("nil channel response for channel %d", args.Channel.ChannelID)
	}
	if channelResp.Channel.FolderID != nil && *channelResp.Channel.FolderID != folderID && !args.Force {
		return command.Result{}, command.NewUserError(fmt.Sprintf(
			"%s is already in another channel folder. Use `channel folder add -f` to reassign it to %q.",
			zulipChannelMention(channelResp.Channel.Name, args.Channel.ChannelID),
			folderName,
		))
	}
	if _, _, err := h.client.UpdateChannel(ctx, args.Channel.ChannelID).FolderID(folderID).Execute(); err != nil {
		return command.Result{}, fmt.Errorf(
			"assign channel %d to folder %d: %w",
			args.Channel.ChannelID,
			folderID,
			err,
		)
	}
	return command.Result{
		Content: fmt.Sprintf(
			"Added folder **%s** to %s.",
			folderName,
			zulipChannelMention(channelResp.Channel.Name, args.Channel.ChannelID),
		),
	}, nil
}

func (h *ChannelHandler) handleFolderRemove(
	ctx context.Context,
	args ChannelFolderRemoveArgs,
) (command.Result, error) {
	if args.Channel.ChannelID <= 0 {
		return command.Result{}, command.NewUserError("channel_id must be a positive integer")
	}
	folderName := strings.TrimSpace(args.FolderName)
	if folderName == "" {
		return command.Result{}, command.NewUserError("folder_name must not be empty")
	}
	folderID, err := h.channelFolderIDByName(ctx, folderName)
	if err != nil {
		return command.Result{}, err
	}
	channelResp, _, err := h.client.GetChannelByID(ctx, args.Channel.ChannelID).Execute()
	if err != nil {
		return command.Result{}, fmt.Errorf("get channel %d: %w", args.Channel.ChannelID, err)
	}
	if channelResp == nil {
		return command.Result{}, fmt.Errorf("nil channel response for channel %d", args.Channel.ChannelID)
	}
	if channelResp.Channel.FolderID == nil {
		return command.Result{}, command.NewUserError(fmt.Sprintf(
			"%s is not in channel folder %q.",
			zulipChannelMention(channelResp.Channel.Name, args.Channel.ChannelID),
			folderName,
		))
	}
	if *channelResp.Channel.FolderID != folderID {
		return command.Result{}, command.NewUserError(fmt.Sprintf(
			"%s is in another channel folder.",
			zulipChannelMention(channelResp.Channel.Name, args.Channel.ChannelID),
		))
	}
	if _, _, err := h.client.UpdateChannel(ctx, args.Channel.ChannelID).FolderIDNone().Execute(); err != nil {
		return command.Result{}, fmt.Errorf("remove folder from channel %d: %w", args.Channel.ChannelID, err)
	}
	return command.Result{
		Content: fmt.Sprintf(
			"Removed folder **%s** from %s.",
			folderName,
			zulipChannelMention(channelResp.Channel.Name, args.Channel.ChannelID),
		),
	}, nil
}

func (h *ChannelHandler) channelFolderIDByName(ctx context.Context, folderName string) (int64, error) {
	resp, _, err := h.client.GetChannelFolders(ctx).IncludeArchived(true).Execute()
	if err != nil {
		return 0, fmt.Errorf("list channel folders: %w", err)
	}
	if resp == nil {
		return 0, errors.New("list channel folders: empty response")
	}
	var matches []zulip.ChannelFolder
	var archivedMatches []zulip.ChannelFolder
	var activeNames []string
	for _, folder := range resp.ChannelFolders {
		if !folder.IsArchived {
			activeNames = append(activeNames, folder.Name)
		}
		switch {
		case folder.Name == folderName && !folder.IsArchived:
			matches = append(matches, folder)
		case folder.Name == folderName:
			archivedMatches = append(archivedMatches, folder)
		}
	}
	switch len(matches) {
	case 0:
		if len(archivedMatches) > 0 {
			return 0, command.NewUserError(fmt.Sprintf("Channel folder %q is archived.", folderName))
		}
		return 0, command.NewUserError(fmt.Sprintf(
			"Unknown channel folder %q. Available folders: %s.",
			folderName,
			formatChannelFolderNames(activeNames),
		))
	case 1:
		return matches[0].ID, nil
	default:
		return 0, command.NewUserError(fmt.Sprintf("Multiple channel folders named %q.", folderName))
	}
}

func formatChannelFolderNames(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	sort.Strings(names)
	for i, name := range names {
		names[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(names, ", ")
}
