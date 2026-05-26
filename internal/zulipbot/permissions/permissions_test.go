package permissions

import (
	"context"
	"errors"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
)

func TestServiceUnknownUserHasNoneRole(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{}}, nil)
	// Unknown user can run PermissionNone commands
	if err := service.Check(context.Background(), model.Actor{UserID: 42}, PermissionNone); err != nil {
		t.Fatalf("unknown user should be allowed PermissionNone: %v", err)
	}
	// Unknown user cannot run PermissionAdmin commands
	if err := service.Check(context.Background(), model.Actor{UserID: 42}, PermissionAdmin); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("unknown user admin check = %v, want ErrDenied", err)
	}
	// Unknown user cannot run PermissionOwner commands
	if err := service.Check(context.Background(), model.Actor{UserID: 42}, PermissionOwner); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("unknown user owner check = %v, want ErrDenied", err)
	}
}

func TestServiceExplicitNoneRoleHasNoPrivilege(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{10: RoleNone}}, nil)
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("explicit none role admin check = %v, want ErrDenied", err)
	}
}

func TestServiceAdminCanRunAdminCommands(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{10: RoleAdmin}}, nil)
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); err != nil {
		t.Fatalf("admin should be allowed PermissionAdmin: %v", err)
	}
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("admin owner check = %v, want ErrDenied", err)
	}
}

func TestServiceZulipOwnerCanRunAllCommands(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{}}, fakeOwnerProvider{ownerID: 10})
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionNone); err != nil {
		t.Fatalf("owner PermissionNone = %v, want nil", err)
	}
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); err != nil {
		t.Fatalf("owner PermissionAdmin = %v, want nil", err)
	}
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); err != nil {
		t.Fatalf("owner PermissionOwner = %v, want nil", err)
	}
}

func TestServiceStoredOwnerRoleDoesNotGrantOwnerPermission(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{10: RoleOwner}}, fakeOwnerProvider{ownerID: 20})

	role, err := service.RoleFor(context.Background(), model.Actor{UserID: 10})
	if err == nil {
		t.Fatalf("RoleFor() role = %q, want error for invalid local owner role", role)
	}
	if !errors.Is(err, ErrPermissionUnavailable) {
		t.Fatalf("RoleFor() error = %v, want ErrPermissionUnavailable", err)
	}

	checkErr := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner)
	if !errors.Is(checkErr, ErrPermissionUnavailable) {
		t.Fatalf("DB owner PermissionOwner = %v, want ErrPermissionUnavailable", checkErr)
	}
}

func TestServiceRoleMatrix(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{roles: map[int64]Role{
		1: RoleNone,
		2: RoleAdmin,
	}}, fakeOwnerProvider{ownerID: 3})

	tests := []struct {
		name       string
		userID     int64
		permission Permission
		wantDenied bool
	}{
		// none role
		{name: "none/PermissionNone", userID: 1, permission: PermissionNone, wantDenied: false},
		{name: "none/PermissionAdmin", userID: 1, permission: PermissionAdmin, wantDenied: true},
		{name: "none/PermissionOwner", userID: 1, permission: PermissionOwner, wantDenied: true},
		// admin role
		{name: "admin/PermissionNone", userID: 2, permission: PermissionNone, wantDenied: false},
		{name: "admin/PermissionAdmin", userID: 2, permission: PermissionAdmin, wantDenied: false},
		{name: "admin/PermissionOwner", userID: 2, permission: PermissionOwner, wantDenied: true},
		// owner role
		{name: "owner/PermissionNone", userID: 3, permission: PermissionNone, wantDenied: false},
		{name: "owner/PermissionAdmin", userID: 3, permission: PermissionAdmin, wantDenied: false},
		{name: "owner/PermissionOwner", userID: 3, permission: PermissionOwner, wantDenied: false},
		// unknown user
		{name: "unknown/PermissionNone", userID: 99, permission: PermissionNone, wantDenied: false},
		{name: "unknown/PermissionAdmin", userID: 99, permission: PermissionAdmin, wantDenied: true},
		{name: "unknown/PermissionOwner", userID: 99, permission: PermissionOwner, wantDenied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := service.Check(context.Background(), model.Actor{UserID: tt.userID}, tt.permission)
			if tt.wantDenied {
				if !errors.Is(err, ErrDenied) {
					t.Fatalf("Check() error = %v, want ErrDenied", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check() failed: %v", err)
			}
		})
	}
}

func TestServicePermissionNoneWorksWithoutDB(t *testing.T) {
	t.Parallel()

	// PermissionNone must work even if DB is unavailable
	service := NewService(fakeRoleStore{err: errors.New("database offline")}, nil)
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionNone); err != nil {
		t.Fatalf("PermissionNone should work without DB: %v", err)
	}
}

func TestServiceFailsClosedWhenRoleStoreFails(t *testing.T) {
	t.Parallel()

	service := NewService(fakeRoleStore{err: errors.New("database offline")}, nil)
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); !errors.Is(
		err,
		ErrPermissionUnavailable,
	) {
		t.Fatalf("Check() error = %v, want ErrPermissionUnavailable", err)
	}
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); !errors.Is(
		err,
		ErrPermissionUnavailable,
	) {
		t.Fatalf("Check() error = %v, want ErrPermissionUnavailable", err)
	}
}

func TestServiceBotOwnerCanRunOwnerOnlyCommands(t *testing.T) {
	t.Parallel()

	// ownerProvider returns userID=10 as the bot owner
	service := NewService(fakeRoleStore{roles: map[int64]Role{}}, fakeOwnerProvider{ownerID: 10})

	// Bot owner should get RoleOwner
	role, err := service.RoleFor(context.Background(), model.Actor{UserID: 10})
	if err != nil {
		t.Fatalf("RoleFor() failed: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("bot owner role = %q, want owner", role)
	}

	// Bot owner can run all permission levels
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); err != nil {
		t.Fatalf("bot owner PermissionOwner = %v, want nil", err)
	}
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); err != nil {
		t.Fatalf("bot owner PermissionAdmin = %v, want nil", err)
	}
}

func TestServiceBotOwnerIsNotDetermineByDB(t *testing.T) {
	t.Parallel()

	// Bot owner from provider should override DB — even if not in DB, they get RoleOwner
	service := NewService(fakeRoleStore{roles: map[int64]Role{}}, fakeOwnerProvider{ownerID: 10})

	// User 10 is not in DB, but is the owner
	role, err := service.RoleFor(context.Background(), model.Actor{UserID: 10})
	if err != nil {
		t.Fatalf("RoleFor() failed: %v", err)
	}
	if role != RoleOwner {
		t.Fatalf("bot owner role = %q, want owner even though not in DB", role)
	}

	// A different user with no DB entry gets RoleNone
	role, err = service.RoleFor(context.Background(), model.Actor{UserID: 99})
	if err != nil {
		t.Fatalf("RoleFor() for non-owner failed: %v", err)
	}
	if role != RoleNone {
		t.Fatalf("non-owner unknown user role = %q, want none", role)
	}
}

func TestServiceMissingBotOwnerFailsClosedForOwnerCommands(t *testing.T) {
	t.Parallel()

	// ownerProvider returns 0 (no owner configured)
	service := NewService(fakeRoleStore{roles: map[int64]Role{}}, fakeOwnerProvider{ownerID: 0})

	// No one can run owner commands
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("owner command with no owner = %v, want ErrDenied", err)
	}
}

func TestServiceDBAdminCannotRunOwnerCommands(t *testing.T) {
	t.Parallel()

	// DB admin should NOT be able to run owner commands even with ownerProvider set
	service := NewService(fakeRoleStore{roles: map[int64]Role{10: RoleAdmin}}, fakeOwnerProvider{ownerID: 20})

	// User 10 is admin in DB; can run admin commands
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionAdmin); err != nil {
		t.Fatalf("DB admin PermissionAdmin = %v, want nil", err)
	}
	// User 10 is NOT the bot owner (20 is); cannot run owner commands
	if err := service.Check(context.Background(), model.Actor{UserID: 10}, PermissionOwner); !errors.Is(
		err,
		ErrDenied,
	) {
		t.Fatalf("DB admin PermissionOwner = %v, want ErrDenied", err)
	}
}

type fakeRoleStore struct {
	roles map[int64]Role
	err   error
}

func (store fakeRoleStore) UserRole(ctx context.Context, userID int64) (Role, bool, error) {
	if store.err != nil {
		return "", false, store.err
	}
	role, ok := store.roles[userID]
	return role, ok, nil
}

type fakeOwnerProvider struct{ ownerID int64 }

func (p fakeOwnerProvider) OwnerUserID() int64 { return p.ownerID }
