package handlers

import (
	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

// Arg structs for the "group" command tree.
// Each distinct leaf action has its own type so the Handle type switch is unambiguous.

type GroupSubscribeArgs struct {
	ShortName string `arg:"course_short_name" desc:"Course identifier to subscribe to"`
}

type GroupUnsubscribeArgs struct {
	KeepChannels bool   `arg:"-k"                desc:"Keep individual channel subscriptions"`
	ShortName    string `arg:"course_short_name" desc:"Course identifier to unsubscribe from"`
}

type GroupCreateArgs struct {
	ShortName string `desc:"Short name for the new group"`
	EmojiName string `desc:"Emoji representing this group, written as :name:" arg:"emoji_name"`
}

type GroupRemoveArgs struct {
	Force     bool   `arg:"-f"         desc:"Archive assigned channels and folders before removing"`
	ShortName string `arg:"short_name" desc:"Group short name to remove"`
}

type GroupMappingListArgs struct{}

// GroupLsArgs covers "group ls" which lists available channel groups.
type GroupLsArgs struct{}

type GroupShowArgs struct {
	ShortName string `arg:"short_name" desc:"Group short name to show details for"`
}

type GroupMappingSetArgs struct {
	ShortName  string     `desc:"Group short name"`
	ZulipGroup zulip.User `desc:"Zulip user group mention"                      arg:"zulip_user_group" mention_only:"true"`
	EmojiName  string     `desc:"Emoji name for the mapping, written as :name:" arg:"emoji_name"`
}

type GroupMappingDisableArgs struct {
	ShortName string `desc:"Group short name to disable"`
}

type GroupChannelAddArgs struct {
	Channel   zulip.Channel `arg:"channel" mention_only:"true" desc:"Zulip channel mention"`
	ShortName string        `                                  desc:"Group short name"`
}

type GroupChannelRemoveArgs struct {
	Channel   zulip.Channel `arg:"channel" mention_only:"true" desc:"Zulip channel mention"`
	ShortName string        `                                  desc:"Group short name"`
}

type GroupChannelCreateArgs struct {
	ChannelName string `desc:"Channel name"`
	ShortName   string `desc:"Group short name"`
}

type GroupFolderAddArgs struct {
	ShortName string `desc:"Group short name"`
}

type GroupFolderRemoveArgs struct {
	ShortName string `desc:"Group short name"`
}

type GroupFolderAssignArgs struct {
	ShortName string `desc:"Group short name"`
}

type GroupFolderUnassignArgs struct {
	ShortName string `desc:"Group short name"`
}

// GroupAnnounceArgs covers "group announce" with no subcommand.
type GroupAnnounceArgs struct{}

type GroupAnnounceSetMessageArgs struct {
	MessageID int64 `arg:"message_id" desc:"Zulip message ID to use as the announcement"`
}

// GroupAnnounceInspectArgs covers "group announce inspect" which takes no arguments.
type GroupAnnounceInspectArgs struct{}

// GroupArgSpec is the full argument schema for the "group" command.
var GroupArgSpec = command.SubcmdSpec{ //nolint:gochecknoglobals // package-level command spec shared across handler methods
	"subscribe":   GroupSubscribeArgs{},
	"unsubscribe": GroupUnsubscribeArgs{},
	"ls":          GroupLsArgs{},
	"show":        GroupShowArgs{},
	"create":      GroupCreateArgs{},
	"remove":      GroupRemoveArgs{},
	"mapping": command.SubcmdSpec{
		"list":    GroupMappingListArgs{},
		"set":     GroupMappingSetArgs{},
		"disable": GroupMappingDisableArgs{},
	},
	"channel": command.SubcmdSpec{
		"add":    GroupChannelAddArgs{},
		"remove": GroupChannelRemoveArgs{},
		"create": GroupChannelCreateArgs{},
	},
	"folder": command.SubcmdSpec{
		"add":      GroupFolderAddArgs{},
		"remove":   GroupFolderRemoveArgs{},
		"assign":   GroupFolderAssignArgs{},
		"unassign": GroupFolderUnassignArgs{},
	},
	"course": command.SubcmdSpec{
		"add":    GroupChannelAddArgs{},
		"remove": GroupChannelRemoveArgs{},
		"create": GroupChannelCreateArgs{},
	},
	"announce": command.SubcmdSpec{
		"":            GroupAnnounceArgs{},
		"set-message": GroupAnnounceSetMessageArgs{},
		"inspect":     GroupAnnounceInspectArgs{},
	},
}
