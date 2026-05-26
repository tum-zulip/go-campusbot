package configsvc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const (
	KeyRestartStartupNotification = "restart_startup_notification"
)

var ErrUnknownKey = errors.New("unknown config key")

type Definition struct {
	Key             string
	Summary         string
	Default         string
	Sensitive       bool
	ReadPermission  zulip.Role
	WritePermission zulip.Role
	Validate        func(string) (string, error)
}

type Value struct {
	Definition Definition
	Value      string
	IsDefault  bool
}

type Service struct {
	repo       *storage.Repository
	permission interface {
		Check(ctx context.Context, actor command.Actor, minRole zulip.Role) error
	}
	definitions map[string]Definition
}

func NewService(repo *storage.Repository, permission interface {
	Check(ctx context.Context, actor command.Actor, minRole zulip.Role) error
},
) *Service {
	return &Service{
		repo:        repo,
		permission:  permission,
		definitions: DefaultDefinitions(),
	}
}

func DefaultDefinitions() map[string]Definition {
	defs := []Definition{
		{
			Key:             KeyRestartStartupNotification,
			Summary:         "Whether the bot sends a restart completion message after coming back online.",
			Default:         "true",
			ReadPermission:  zulip.RoleAdmin,
			WritePermission: zulip.RoleAdmin,
			Validate:        validateBool,
		},
	}

	result := make(map[string]Definition, len(defs))
	for _, def := range defs {
		result[def.Key] = def
	}
	return result
}

// AddDefinition registers or overrides a configuration definition.
func (service *Service) AddDefinition(def Definition) {
	if service == nil || def.Key == "" {
		return
	}
	service.definitions[def.Key] = def
}

func (service *Service) Bool(ctx context.Context, key string) (bool, error) {
	value, err := service.GetRaw(ctx, key)
	if err != nil {
		return false, err
	}
	parsed, err := strconv.ParseBool(value.Value)
	if err != nil {
		return false, fmt.Errorf("stored config %q is not a bool: %w", key, err)
	}
	return parsed, nil
}

func (service *Service) Get(ctx context.Context, actor command.Actor, key string) (Value, error) {
	value, err := service.GetRaw(ctx, key)
	if err != nil {
		return Value{}, err
	}
	if err := service.permission.Check(ctx, actor, value.Definition.ReadPermission); err != nil {
		return Value{}, err
	}
	return value, nil
}

func (service *Service) GetRaw(ctx context.Context, key string) (Value, error) {
	def, ok := service.definitions[key]
	if !ok {
		return Value{}, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	stored, ok, err := service.repo.ConfigValue(ctx, key)
	if err != nil {
		return Value{}, err
	}
	if !ok {
		return Value{Definition: def, Value: def.Default, IsDefault: true}, nil
	}
	normalized, err := def.Validate(stored)
	if err != nil {
		return Value{}, fmt.Errorf("stored config %q is invalid: %w", key, err)
	}
	return Value{Definition: def, Value: normalized}, nil
}

func (service *Service) List(ctx context.Context, actor command.Actor) ([]Value, error) {
	keys := make([]string, 0, len(service.definitions))
	for key := range service.definitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := make([]Value, 0, len(keys))
	for _, key := range keys {
		value, err := service.Get(ctx, actor, key)
		if errors.Is(err, command.ErrDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func (service *Service) Set(
	ctx context.Context,
	actor command.Actor,
	messageID int64,
	key string,
	value string,
) (Value, Value, error) {
	def, ok := service.definitions[key]
	if !ok {
		return Value{}, Value{}, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if err := service.permission.Check(ctx, actor, def.WritePermission); err != nil {
		return Value{}, Value{}, err
	}
	oldValue, err := service.GetRaw(ctx, key)
	if err != nil {
		return Value{}, Value{}, err
	}
	normalized, err := def.Validate(value)
	if err != nil {
		return Value{}, Value{}, command.NewUserError(fmt.Sprintf("Invalid value for `%s`: %v", key, err))
	}
	newValue := Value{Definition: def, Value: normalized}
	if err := service.repo.SetConfigValue(ctx, storage.ConfigChange{
		Key:              key,
		Value:            normalized,
		ActorUserID:      actor.UserID,
		MessageID:        messageID,
		OldValueRedacted: Redact(oldValue),
		NewValueRedacted: Redact(newValue),
	}); err != nil {
		return Value{}, Value{}, err
	}
	return oldValue, newValue, nil
}

func Redact(value Value) string {
	if value.Definition.Sensitive {
		if value.Value == "" {
			return "<empty>"
		}
		return "<redacted>"
	}
	return value.Value
}

func validateBool(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return "true", nil
	case "false", "0", "no", "off":
		return "false", nil
	default:
		return "", errors.New("expected a boolean value")
	}
}
