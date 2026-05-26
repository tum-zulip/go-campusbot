package handlers

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestConfigHandlerAdminCanListGetAndSet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openHandlerTestRepository(t)
	defer repo.Close()
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := configsvc.NewService(repo, permissions.NewService(repo, nil))
	handler := NewConfigHandler(service)
	actor := model.Actor{UserID: 10}

	setResult, err := handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"set", configsvc.KeyRestartStartupNotification, "true"}},
		Actor:      actor,
		MessageID:  100,
	})
	if err != nil {
		t.Fatalf("Handle(set) failed: %v", err)
	}
	if !strings.Contains(setResult.Content, "Configuration updated") {
		t.Fatalf("set content = %q", setResult.Content)
	}

	getResult, err := handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"get", configsvc.KeyRestartStartupNotification}},
		Actor:      actor,
	})
	if err != nil {
		t.Fatalf("Handle(get) failed: %v", err)
	}
	if !strings.Contains(getResult.Content, "true") {
		t.Fatalf("get content = %q", getResult.Content)
	}

	listResult, err := handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"list"}},
		Actor:      actor,
	})
	if err != nil {
		t.Fatalf("Handle(list) failed: %v", err)
	}
	if !strings.Contains(listResult.Content, configsvc.KeyRestartStartupNotification) {
		t.Fatalf("list content = %q", listResult.Content)
	}
}

func TestConfigHandlerReportsSafeUserErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openHandlerTestRepository(t)
	defer repo.Close()
	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	service := configsvc.NewService(repo, permissions.NewService(repo, nil))
	handler := NewConfigHandler(service)

	_, err := handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"get", "does_not_exist"}},
		Actor:      model.Actor{UserID: 10},
	})
	var userErr command.UserError
	if !errors.As(err, &userErr) || userErr.Message != "Unknown configuration key." {
		t.Fatalf("Handle(get unknown) error = %v", err)
	}

	_, err = handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"set", configsvc.KeyRestartStartupNotification, "maybe"}},
		Actor:      model.Actor{UserID: 10},
	})
	if !errors.As(err, &userErr) || !strings.Contains(userErr.Message, "Invalid value") {
		t.Fatalf("Handle(set invalid) error = %v", err)
	}
}

func TestConfigHandlerFailsClosedWhenPermissionsUnavailable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openHandlerTestRepository(t)
	defer repo.Close()
	service := configsvc.NewService(repo, failingPermission{})
	handler := NewConfigHandler(service)

	_, err := handler.Handle(ctx, command.Request{
		Invocation: command.Invocation{Name: "config", Args: []string{"list"}},
		Actor:      model.Actor{UserID: 10},
	})
	var userErr command.UserError
	if !errors.As(err, &userErr) || userErr.Message != "I cannot verify permissions right now, so I will not access configuration." {
		t.Fatalf("Handle(list) error = %v", err)
	}
}

type failingPermission struct{}

func (failingPermission) Check(ctx context.Context, actor model.Actor, permission permissions.Permission) error {
	return permissions.ErrPermissionUnavailable
}

func openHandlerTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
