package configsvc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestServiceValidatesAndPersistsConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := NewService(repo, permissions.NewService(repo, nil))
	actor := model.Actor{UserID: 10}

	_, newValue, err := service.Set(ctx, actor, 123, KeyRestartStartupNotification, "yes")
	if err != nil {
		t.Fatalf("Set() failed: %v", err)
	}
	if newValue.Value != "true" {
		t.Fatalf("normalized value = %q, want true", newValue.Value)
	}

	_, _, err = service.Set(ctx, actor, 124, KeyRestartStartupNotification, "maybe")
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
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := NewService(repo, permissions.NewService(repo, nil))
	actor := model.Actor{UserID: 10}

	if _, err := service.Get(ctx, actor, "not_a_real_key"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Get() error = %v, want ErrUnknownKey", err)
	}
	if _, _, err := service.Set(ctx, actor, 123, "not_a_real_key", "value"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Set() error = %v, want ErrUnknownKey", err)
	}
	_, _, err := service.Set(ctx, actor, 124, KeyRestartStartupNotification, "maybe")
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
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := NewService(repo, permissions.NewService(repo, nil))
	service.definitions["secret_token"] = Definition{
		Key:             "secret_token",
		Summary:         "test secret",
		Default:         "default-secret",
		Sensitive:       true,
		ReadPermission:  permissions.PermissionAdmin,
		WritePermission: permissions.PermissionAdmin,
		Validate: func(value string) (string, error) {
			if value == "" {
				return "", errors.New("must not be empty")
			}
			return value, nil
		},
	}

	oldValue, newValue, err := service.Set(ctx, model.Actor{UserID: 10}, 500, "secret_token", "super-secret-value")
	if err != nil {
		t.Fatalf("Set() sensitive failed: %v", err)
	}
	if Redact(oldValue) != "<redacted>" || Redact(newValue) != "<redacted>" {
		t.Fatalf("redacted old/new = %q/%q", Redact(oldValue), Redact(newValue))
	}
	records, err := repo.AuditRecords(ctx)
	if err != nil {
		t.Fatalf("AuditRecords() failed: %v", err)
	}
	last := records[len(records)-1]
	if last.Action != "config.set" || last.Target != "secret_token" || last.NewValue != "<redacted>" {
		t.Fatalf("audit record = %#v", last)
	}
}

func TestServiceProtectsWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	service := NewService(repo, permissions.NewService(repo, nil))

	// Unknown user (none role) cannot write config (requires admin)
	_, _, err := service.Set(ctx, model.Actor{UserID: 20}, 123, KeyRestartStartupNotification, "true")
	if !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("regular user Set() error = %v, want ErrDenied", err)
	}
}

func TestServiceProtectsSensitiveConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openConfigTestRepository(t)
	defer repo.Close()
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := NewService(repo, permissions.NewService(repo, nil))
	service.definitions["secret_token"] = Definition{
		Key:             "secret_token",
		Default:         "default-secret",
		Sensitive:       true,
		ReadPermission:  permissions.PermissionOwner,
		WritePermission: permissions.PermissionOwner,
		Validate: func(value string) (string, error) {
			return value, nil
		},
	}

	// Admin can't read owner-only sensitive config
	_, err := service.Get(ctx, model.Actor{UserID: 10}, "secret_token")
	if !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("admin sensitive Get() error = %v, want ErrDenied", err)
	}
	_, _, err = service.Set(ctx, model.Actor{UserID: 10}, 123, "secret_token", "value")
	if !errors.Is(err, permissions.ErrDenied) {
		t.Fatalf("admin sensitive Set() error = %v, want ErrDenied", err)
	}
}

func openConfigTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
