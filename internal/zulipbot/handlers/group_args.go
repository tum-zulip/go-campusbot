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
	EmojiName string `desc:"Emoji representing this group"`
}

// GroupAvailableArgs covers "group available" which takes no arguments.
type GroupAvailableArgs struct{}

type GroupMappingListArgs struct{}

type GroupMappingSetArgs struct {
	ShortName    string `desc:"Group short name"`
	ZulipGroupID int64  `desc:"Zulip user group ID"`
	EmojiName    string `desc:"Emoji name for the mapping"`
}

type GroupMappingDisableArgs struct {
	ShortName string `desc:"Group short name to disable"`
}

type GroupChannelAddArgs struct {
	Channel   zulip.Channel `desc:"Zulip channel"`
	ShortName string        `desc:"Group short name"`
}

type GroupChannelRemoveArgs struct {
	Channel   zulip.Channel `desc:"Zulip channel"`
	ShortName string        `desc:"Group short name"`
}

type GroupChannelCreateArgs struct {
	ChannelName string `desc:"Channel name"`
	ShortName   string `desc:"Group short name"`
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
	"create":      GroupCreateArgs{},
	"available":   GroupAvailableArgs{},
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
