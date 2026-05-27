package channelgroup

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/tum-zulip/go-zulip/zulip"
)

// GroupService provides simple user-oriented subscribe/unsubscribe for channel groups.
// It wraps the existing channelGroups API plus the upstream Zulip user-group
// endpoint, so admin diagnostics can list groups visible in Zulip itself.
type GroupService struct {
	client Client
}

// NewGroupService creates a new GroupService wrapping the given channel-group
// Client. The Client must expose both the channel-group endpoints and the
// upstream Zulip user-group endpoints (`Client` already embeds both).
func NewGroupService(client Client) *GroupService {
	return &GroupService{client: client}
}

// ZulipUserGroupSummary is a compact view of a Zulip user group used by
// admin diagnostic commands. It carries just enough information to identify
// a group and see its member count. Local import state is intentionally
// omitted: admins use the Zulip user group ID directly when configuring
// emoji mappings, and the bot handles local-import bookkeeping internally.
type ZulipUserGroupSummary struct {
	ID            int64
	Name          string
	Description   string
	MemberCount   int
	IsSystemGroup bool
}

// CreateChannelGroup creates a Zulip user group and tracks it locally as a
// channel group. If a later step fails after the user group is created, the
// lower-level API rolls back the created resources where Zulip exposes a
// reversal operation.
func (s *GroupService) CreateChannelGroup(ctx context.Context, name string, createChannelFolder bool) (int64, error) {
	ownUserResp, _, err := s.client.GetOwnUser(ctx).Execute()
	if err != nil {
		return 0, fmt.Errorf("get own Zulip user for channel group %q: %w", name, err)
	}
	if ownUserResp == nil || ownUserResp.User.UserID <= 0 {
		return 0, fmt.Errorf("get own Zulip user for channel group %q: missing user ID", name)
	}

	resp, _, err := s.client.CreateChannelGroup(ctx).
		CreateChannelFolder(createChannelFolder).
		Name(name).
		InitialSubscribers(zulip.UserIDsAsPrincipals(ownUserResp.User.UserID)).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("create channel group %q: %w", name, err)
	}
	return resp.ChannelGroupID, nil
}

// DeleteChannelGroup removes the local channel group and deactivates its
// backing Zulip user group. It is intended for rollback of groups created by
// this bot.
func (s *GroupService) DeleteChannelGroup(ctx context.Context, channelGroupID int64) error {
	return s.client.DeleteChannelGroup(ctx, channelGroupID)
}

// SubscribeUser subscribes a single user to the specified channel group.
func (s *GroupService) SubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error {
	_, _, err := s.client.SubscribeToChannelGroup(ctx, channelGroupID).
		Principals(zulip.Principals{UserIDs: &[]int64{userID}}).
		Execute()
	if err != nil {
		return fmt.Errorf("subscribe user %d to channel group %d: %w", userID, channelGroupID, err)
	}
	return nil
}

// UnsubscribeUser unsubscribes a single user from the channel group and removes them from existing channels.
func (s *GroupService) UnsubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error {
	_, _, err := s.client.UnsubscribeFromChannelGroup(ctx, channelGroupID).
		Principals(zulip.Principals{UserIDs: &[]int64{userID}}).
		Execute()
	if err != nil {
		return fmt.Errorf("unsubscribe user %d from channel group %d: %w", userID, channelGroupID, err)
	}
	return nil
}

// ChannelGroupExists reports whether the channel group with the given ID exists
// in the local channelgroup database. Returns (false, nil) when missing.
func (s *GroupService) ChannelGroupExists(ctx context.Context, channelGroupID int64) (bool, error) {
	_, _, err := s.client.GetChannelGroup(ctx, channelGroupID).Execute()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrChannelGroupNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("check channel group %d exists: %w", channelGroupID, err)
}

// ListChannelGroups returns the channel groups currently known to the bot.
// Names are hydrated from the backing Zulip user groups; a failure to hydrate
// names is reported as an error so callers can distinguish "no groups imported"
// from "lookup failed".
//
// This method is retained for internal/debug use; it is not exposed as a bot
// command because admins interact with Zulip user group IDs directly and the
// bot manages local import state on their behalf.
func (s *GroupService) ListChannelGroups(ctx context.Context) ([]ChannelGroup, error) {
	resp, _, err := s.client.GetChannelGroups(ctx).Execute()
	if err != nil {
		return nil, fmt.Errorf("list channel groups: %w", err)
	}
	return resp.ChannelGroups, nil
}

// ListZulipUserGroups returns the user groups visible to the bot account in
// Zulip. Deactivated user groups are excluded.
func (s *GroupService) ListZulipUserGroups(ctx context.Context) ([]ZulipUserGroupSummary, error) {
	resp, _, err := s.client.GetUserGroups(ctx).IncludeDeactivatedGroups(false).Execute()
	if err != nil {
		return nil, fmt.Errorf("list zulip user groups: %w", err)
	}

	summaries := make([]ZulipUserGroupSummary, 0, len(resp.UserGroups))
	for _, group := range resp.UserGroups {
		if group.Deactivated || group.IsSystemGroup {
			continue
		}
		summaries = append(summaries, ZulipUserGroupSummary{
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

// ImportZulipUserGroup records the given Zulip user group ID as a channel
// group in the bot's local database, without creating a new Zulip user group.
// This is the auto-import path used when an admin configures an emoji mapping
// for a Zulip user group the bot can see but has not yet tracked locally.
//
// The operation is idempotent: importing an already-tracked group is a no-op.
// Channel membership is left empty; it must be populated by the regular
// channel-group mechanisms (UpdateChannelGroupChannels) if needed.
//
// The caller is expected to have verified that the user group is visible in
// Zulip (typically via [ListZulipUserGroups]); ImportZulipUserGroup does not
// re-validate visibility itself.
func (s *GroupService) ImportZulipUserGroup(ctx context.Context, userGroupID int64) error {
	return s.client.ImportZulipUserGroup(ctx, userGroupID)
}

// CreateChannelAndAddToGroup creates a new Zulip channel with the given name,
// subscribes the bot account to it, and adds the channel to the specified group.
// Returns the new channel's ID on success.
func (s *GroupService) CreateChannelAndAddToGroup(
	ctx context.Context,
	channelName string,
	channelGroupID int64,
) (int64, error) {
	ownUserResp, _, err := s.client.GetOwnUser(ctx).Execute()
	if err != nil {
		return 0, fmt.Errorf("get own Zulip user for channel creation: %w", err)
	}
	if ownUserResp == nil || ownUserResp.User.UserID <= 0 {
		return 0, errors.New("get own Zulip user for channel creation: missing user ID")
	}

	channelResp, _, err := s.client.CreateChannel(ctx).
		Name(channelName).
		Subscribers([]int64{ownUserResp.User.UserID}).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("create channel %q: %w", channelName, err)
	}

	if err := s.AddChannelToGroup(ctx, channelGroupID, channelResp.ID); err != nil {
		return 0, fmt.Errorf("add channel %d to group %d: %w", channelResp.ID, channelGroupID, err)
	}
	return channelResp.ID, nil
}

// AddChannelToGroup adds a channel to a channel group, subscribing all current group members to it.
func (s *GroupService) AddChannelToGroup(ctx context.Context, channelGroupID int64, channelID int64) error {
	_, _, err := s.client.UpdateChannelGroupChannels(ctx, channelGroupID).
		Add([]int64{channelID}).
		Execute()
	if err != nil {
		return fmt.Errorf("add channel %d to channel group %d: %w", channelID, channelGroupID, err)
	}
	return nil
}

// RemoveChannelFromGroup removes a channel from a channel group.
func (s *GroupService) RemoveChannelFromGroup(ctx context.Context, channelGroupID int64, channelID int64) error {
	_, _, err := s.client.UpdateChannelGroupChannels(ctx, channelGroupID).
		Delete([]int64{channelID}).
		Execute()
	if err != nil {
		return fmt.Errorf("remove channel %d from channel group %d: %w", channelID, channelGroupID, err)
	}
	return nil
}

// AddFolderToGroup creates a Zulip channel folder for the group and assigns
// the group's current channels to it. If a folder already exists, it assigns
// the current channels to the existing folder.
func (s *GroupService) AddFolderToGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := s.client.UpdateChannelGroupFolder(ctx, channelGroupID).Add().Execute()
	if err != nil {
		return fmt.Errorf("add channel folder to channel group %d: %w", channelGroupID, err)
	}
	return nil
}

// RemoveFolderFromGroup archives the group's Zulip channel folder and removes
// the folder association from the local channel group.
func (s *GroupService) RemoveFolderFromGroup(ctx context.Context, channelGroupID int64) error {
	_, _, err := s.client.UpdateChannelGroupFolder(ctx, channelGroupID).Remove().Execute()
	if err != nil {
		return fmt.Errorf("remove channel folder from channel group %d: %w", channelGroupID, err)
	}
	return nil
}

// AssignFolderToGroupChannels assigns the group's current channels to its
// existing Zulip channel folder.
func (s *GroupService) AssignFolderToGroupChannels(ctx context.Context, channelGroupID int64) error {
	_, _, err := s.client.UpdateChannelGroupFolder(ctx, channelGroupID).Assign().Execute()
	if err != nil {
		return fmt.Errorf("assign channel folder for channel group %d: %w", channelGroupID, err)
	}
	return nil
}

// UnassignFolderFromChannels unassigns channels from the group's existing
// Zulip channel folder, but keeps the folder associated with the group.
func (s *GroupService) UnassignFolderFromChannels(ctx context.Context, channelGroupID int64) error {
	_, _, err := s.client.UpdateChannelGroupFolder(ctx, channelGroupID).Unassign().Execute()
	if err != nil {
		return fmt.Errorf("unassign channel folder for channel group %d: %w", channelGroupID, err)
	}
	return nil
}

// UnsubscribeUserKeepChannels removes a user from the channel group future updates
// but keeps their existing channel memberships.
func (s *GroupService) UnsubscribeUserKeepChannels(ctx context.Context, userID int64, channelGroupID int64) error {
	_, _, err := s.client.UnsubscribeFromChannelGroup(ctx, channelGroupID).
		Principals(zulip.Principals{UserIDs: &[]int64{userID}}).
		KeepChannels().
		Execute()
	if err != nil {
		return fmt.Errorf("unsubscribe user %d from channel group %d (keep channels): %w", userID, channelGroupID, err)
	}
	return nil
}
