package permissions

import (
	"context"
	"errors"
	"fmt"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
)

type Role string

const (
	RoleNone  Role = "none"
	RoleAdmin Role = "admin"
	RoleOwner Role = "owner"
)

type Permission string

const (
	PermissionNone  Permission = "none"  // everyone
	PermissionAdmin Permission = "admin" // admin or owner
	PermissionOwner Permission = "owner" // owner only
)

var (
	ErrDenied                = errors.New("permission denied")
	ErrPermissionUnavailable = errors.New("permission unavailable")
)

type Store interface {
	UserRole(ctx context.Context, userID int64) (Role, bool, error)
}

// OwnerProvider provides the dynamically resolved bot owner user ID.
type OwnerProvider interface {
	// OwnerUserID returns the Zulip user ID of the bot owner, or 0 if unresolved.
	OwnerUserID() int64
}

type Service struct {
	store         Store
	ownerProvider OwnerProvider
}

func NewService(store Store, ownerProvider OwnerProvider) *Service {
	return &Service{store: store, ownerProvider: ownerProvider}
}

func (service *Service) RoleFor(ctx context.Context, actor model.Actor) (Role, error) {
	if actor.UserID <= 0 {
		return RoleNone, nil
	}

	// Check ownerProvider first — if the actor is the bot owner, return RoleOwner.
	if service.ownerProvider != nil {
		if ownerID := service.ownerProvider.OwnerUserID(); ownerID > 0 && actor.UserID == ownerID {
			return RoleOwner, nil
		}
	}

	if service.store == nil {
		return RoleNone, ErrPermissionUnavailable
	}

	role, ok, err := service.store.UserRole(ctx, actor.UserID)
	if err != nil {
		return RoleNone, fmt.Errorf("%w: load role for Zulip user %d: %v", ErrPermissionUnavailable, actor.UserID, err)
	}
	if !ok {
		return RoleNone, nil
	}
	if !role.Valid() {
		return RoleNone, fmt.Errorf("%w: invalid stored role for Zulip user %d", ErrPermissionUnavailable, actor.UserID)
	}
	if role == RoleOwner {
		return RoleNone, fmt.Errorf("%w: local owner role for Zulip user %d is invalid; owner is Zulip-derived", ErrPermissionUnavailable, actor.UserID)
	}
	return role, nil
}

func (service *Service) Check(ctx context.Context, actor model.Actor, permission Permission) error {
	if permission == PermissionNone {
		return nil
	}
	role, err := service.RoleFor(ctx, actor)
	if err != nil {
		return err
	}
	if !role.Allows(permission) {
		return fmt.Errorf("%w: %s requires %s", ErrDenied, permission, RequiredRole(permission))
	}
	return nil
}

func ParseRole(value string) (Role, error) {
	switch Role(value) {
	case RoleNone, RoleAdmin, RoleOwner:
		return Role(value), nil
	default:
		return "", fmt.Errorf("unknown role %q: valid roles are none, admin, owner", value)
	}
}

func (role Role) Valid() bool {
	switch role {
	case RoleNone, RoleAdmin, RoleOwner:
		return true
	default:
		return false
	}
}

func (role Role) Allows(permission Permission) bool {
	return role.rank() >= RequiredRole(permission).rank()
}

func RequiredRole(permission Permission) Role {
	switch permission {
	case PermissionNone:
		return RoleNone
	case PermissionAdmin:
		return RoleAdmin
	case PermissionOwner:
		return RoleOwner
	default:
		return RoleOwner
	}
}

func (role Role) rank() int {
	switch role {
	case RoleOwner:
		return 2
	case RoleAdmin:
		return 1
	default:
		return 0
	}
}
