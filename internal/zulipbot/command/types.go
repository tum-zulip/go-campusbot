package command

import (
	"errors"
	"fmt"
)

type Actor struct {
	UserID   int64
	Email    string
	FullName string
}

type ReplyKind string

const (
	ReplyKindChannel ReplyKind = "channel"
	ReplyKindDirect  ReplyKind = "direct"
)

type ReplyTarget struct {
	Kind      ReplyKind
	ChannelID int64
	Topic     string
	UserIDs   []int64
}

func (target ReplyTarget) Validate() error {
	switch target.Kind {
	case ReplyKindChannel:
		if target.ChannelID == 0 {
			return errors.New("channel reply target requires a channel ID")
		}
		if target.Topic == "" {
			return errors.New("channel reply target requires a topic")
		}
	case ReplyKindDirect:
		if len(target.UserIDs) == 0 {
			return errors.New("direct reply target requires at least one user ID")
		}
	default:
		return fmt.Errorf("unknown reply target kind %q", target.Kind)
	}
	return nil
}
