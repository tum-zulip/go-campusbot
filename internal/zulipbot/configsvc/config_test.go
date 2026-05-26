package configsvc_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestServiceValidatesAndPersistsConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, fakeConfigPerm{10: zulip.RoleAdmin})
	actor := command.Actor{UserID: 10}

	_, newValue, err := service.Set(ctx, actor, 123, configsvc.KeyRestartStartupNotification, "yes")
	if err != nil {
		t.Fatalf("Set() failed: %v", err)
	}
	if newValue.Value != "true" {
		t.Fatalf("normalized value = %q, want true", newValue.Value)
	}

	_, _, err = service.Set(ctx, actor, 124, configsvc.KeyRestartStartupNotification, "maybe")
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("invalid bool error = %v, want command.UserError", err)
	}
}

func TestServiceRejectsUnknownAndInvalidConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, fakeConfigPerm{10: zulip.RoleAdmin})
	actor := command.Actor{UserID: 10}

	if _, err := service.Get(ctx, actor, "not_a_real_key"); !errors.Is(err, configsvc.ErrUnknownKey) {
		t.Fatalf("Get() error = %v, want ErrUnknownKey", err)
	}
	if _, _, err := service.Set(ctx, actor, 123, "not_a_real_key", "value"); !errors.Is(err, configsvc.ErrUnknownKey) {
		t.Fatalf("Set() error = %v, want ErrUnknownKey", err)
	}
	_, _, err := service.Set(ctx, actor, 124, configsvc.KeyRestartStartupNotification, "maybe")
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("invalid bool error = %v, want command.UserError", err)
	}
}

func TestServiceMasksSensitiveValuesAndAuditsWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, fakeConfigPerm{10: zulip.RoleAdmin})
	service.AddDefinition(configsvc.Definition{
		Key:             "secret_token",
		Summary:         "test secret",
		Default:         "default-secret",
		Sensitive:       true,
		ReadPermission:  zulip.RoleAdmin,
		WritePermission: zulip.RoleAdmin,
		Validate: func(value string) (string, error) {
			if value == "" {
				return "", errors.New("must not be empty")
			}
			return value, nil
		},
	})

	const redacted = "<redacted>"

	oldValue, newValue, err := service.Set(ctx, command.Actor{UserID: 10}, 500, "secret_token", "super-secret-value")
	if err != nil {
		t.Fatalf("Set() sensitive failed: %v", err)
	}
	if configsvc.Redact(oldValue) != redacted || configsvc.Redact(newValue) != redacted {
		t.Fatalf("redacted old/new = %q/%q", configsvc.Redact(oldValue), configsvc.Redact(newValue))
	}
	records, err := repo.AuditRecords(ctx)
	if err != nil {
		t.Fatalf("AuditRecords() failed: %v", err)
	}
	last := records[len(records)-1]
	if last.Action != "config.set" || last.Target != "secret_token" || last.NewValue != redacted {
		t.Fatalf("audit record = %#v", last)
	}
}

func TestServiceProtectsWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, fakeConfigPerm{})

	// Unknown user (member role) cannot write config (requires admin)
	_, _, err := service.Set(ctx, command.Actor{UserID: 20}, 123, configsvc.KeyRestartStartupNotification, "true")
	if !errors.Is(err, command.ErrDenied) {
		t.Fatalf("regular user Set() error = %v, want ErrDenied", err)
	}
}

func TestServiceProtectsSensitiveConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, fakeConfigPerm{10: zulip.RoleAdmin})
	service.AddDefinition(configsvc.Definition{
		Key:             "secret_token",
		Default:         "default-secret",
		Sensitive:       true,
		ReadPermission:  zulip.RoleOwner,
		WritePermission: zulip.RoleOwner,
		Validate: func(value string) (string, error) {
			return value, nil
		},
	})

	// Admin can't read owner-only sensitive config
	_, err := service.Get(ctx, command.Actor{UserID: 10}, "secret_token")
	if !errors.Is(err, command.ErrDenied) {
		t.Fatalf("admin sensitive Get() error = %v, want ErrDenied", err)
	}
	_, _, err = service.Set(ctx, command.Actor{UserID: 10}, 123, "secret_token", "value")
	if !errors.Is(err, command.ErrDenied) {
		t.Fatalf("admin sensitive Set() error = %v, want ErrDenied", err)
	}
}

// fakeConfigPerm maps user IDs to Zulip roles; unmapped users get RoleMember.
type fakeConfigPerm map[int64]zulip.Role

func (f fakeConfigPerm) Check(_ context.Context, actor command.Actor, minRole zulip.Role) error {
	if minRole == 0 {
		return nil
	}
	role, ok := f[actor.UserID]
	if !ok {
		role = zulip.RoleMember
	}
	if role <= minRole {
		return nil
	}
	return command.ErrDenied
}

func openConfigTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
