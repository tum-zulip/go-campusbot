package zulipbot

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
	KeyAnnouncementChannelID      = "announcement.channel_id"
	KeyAnnouncementTopic          = "announcement.topic"
)

var errUnknownConfigKey = errors.New("unknown config key")

type configDef struct {
	Key             string
	Summary         string
	Default         string
	Sensitive       bool
	ReadPermission  zulip.Role
	WritePermission zulip.Role
	Validate        func(string) (string, error)
}

type configValue struct {
	Def       configDef
	Value     string
	IsDefault bool
}

//nolint:gochecknoglobals // static command schema, matches helpMeta/statusMeta/restartMeta pattern
var configDefs = map[string]configDef{
	KeyRestartStartupNotification: {
		Key:             KeyRestartStartupNotification,
		Summary:         "Whether the bot sends a restart completion message after coming back online.",
		Default:         "true",
		ReadPermission:  zulip.RoleAdmin,
		WritePermission: zulip.RoleAdmin,
		Validate:        validateConfigBool,
	},
	KeyAnnouncementChannelID: {
		Key:             KeyAnnouncementChannelID,
		Summary:         "Channel ID for the channel group announcement message.",
		Default:         "",
		ReadPermission:  zulip.RoleAdmin,
		WritePermission: zulip.RoleAdmin,
		Validate:        validateConfigPositiveInt64,
	},
	KeyAnnouncementTopic: {
		Key:             KeyAnnouncementTopic,
		Summary:         "Topic for the channel group announcement message.",
		Default:         "",
		ReadPermission:  zulip.RoleAdmin,
		WritePermission: zulip.RoleAdmin,
		Validate:        validateConfigNonEmptyString,
	},
}

type configGetArgs struct {
	Key string `desc:"Configuration key to read"`
}

type configSetArgs struct {
	Key   string `desc:"Configuration key to update"`
	Value string `desc:"New value"`
}

//nolint:gochecknoglobals // static command metadata, matches helpMeta/statusMeta/restartMeta pattern
var configMeta = command.Metadata{
	Name:       "config",
	Summary:    "Read or update bot configuration.",
	Usage:      "config <list|get|set> [key] [value]",
	Permission: zulip.RoleAdmin,
	Privileged: true,
	ArgSpec: command.SubcmdSpec{
		"list": command.NoArgs{},
		"get":  configGetArgs{},
		"set":  configSetArgs{},
	},
}

func (bot *Bot) getConfig(ctx context.Context, key string) (configValue, error) {
	def, ok := configDefs[key]
	if !ok {
		return configValue{}, fmt.Errorf("%w: %s", errUnknownConfigKey, key)
	}
	stored, ok, err := bot.repo.ConfigValue(ctx, key)
	if err != nil {
		return configValue{}, err
	}
	if !ok {
		return configValue{Def: def, Value: def.Default, IsDefault: true}, nil
	}
	normalized, err := def.Validate(stored)
	if err != nil {
		return configValue{}, fmt.Errorf("stored config %q is invalid: %w", key, err)
	}
	return configValue{Def: def, Value: normalized}, nil
}

func (bot *Bot) boolConfig(ctx context.Context, key string) (bool, error) {
	v, err := bot.getConfig(ctx, key)
	if err != nil {
		return false, err
	}
	parsed, err := strconv.ParseBool(v.Value)
	if err != nil {
		return false, fmt.Errorf("stored config %q is not a bool: %w", key, err)
	}
	return parsed, nil
}

func (bot *Bot) AnnouncementChannelID(ctx context.Context) (int64, bool, error) {
	v, err := bot.getConfig(ctx, KeyAnnouncementChannelID)
	if err != nil {
		return 0, false, err
	}
	if v.IsDefault || v.Value == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func (bot *Bot) AnnouncementTopic(ctx context.Context) (string, bool, error) {
	v, err := bot.getConfig(ctx, KeyAnnouncementTopic)
	if err != nil {
		return "", false, err
	}
	if v.IsDefault || v.Value == "" {
		return "", false, nil
	}
	return v.Value, true, nil
}

func (bot *Bot) setConfig(
	ctx context.Context,
	actor command.Actor,
	messageID int64,
	key, value string,
) (configValue, configValue, error) {
	def, ok := configDefs[key]
	if !ok {
		return configValue{}, configValue{}, fmt.Errorf("%w: %s", errUnknownConfigKey, key)
	}
	oldValue, err := bot.getConfig(ctx, key)
	if err != nil {
		return configValue{}, configValue{}, err
	}
	normalized, err := def.Validate(value)
	if err != nil {
		return configValue{}, configValue{}, command.NewUserError(fmt.Sprintf("Invalid value for `%s`: %v", key, err))
	}
	newValue := configValue{Def: def, Value: normalized}
	if err := bot.repo.SetConfigValue(ctx, storage.ConfigChange{
		Key:              key,
		Value:            normalized,
		ActorUserID:      actor.UserID,
		MessageID:        messageID,
		OldValueRedacted: redactConfig(oldValue),
		NewValueRedacted: redactConfig(newValue),
	}); err != nil {
		return configValue{}, configValue{}, err
	}
	return oldValue, newValue, nil
}

func redactConfig(v configValue) string {
	if v.Def.Sensitive {
		if v.Value == "" {
			return "<empty>"
		}
		return "<redacted>"
	}
	return v.Value
}

func (bot *Bot) handleConfig(ctx context.Context, req command.Request) command.Result {
	parsed, err := bot.argParser.Parse(ctx, configMeta.ArgSpec, req.Invocation.Args)
	if err != nil {
		var userErr command.UserError
		if errors.As(err, &userErr) {
			return command.Result{Content: userErr.Message}
		}
		bot.logger.ErrorContext(ctx, "config arg parsing failed", "error", err)
		return command.Result{Content: "Command failed because of an internal error."}
	}
	switch args := parsed.(type) {
	case command.NoArgs:
		return bot.configList(ctx, req.Actor)
	case configGetArgs:
		return bot.configGet(ctx, req.Actor, args.Key)
	case configSetArgs:
		return bot.configSet(ctx, req.Actor, req.MessageID, args.Key, args.Value)
	}
	return command.Result{Content: "Usage: `config <list|get|set> [key] [value]`"}
}

func (bot *Bot) configList(ctx context.Context, actor command.Actor) command.Result {
	keys := make([]string, 0, len(configDefs))
	for key := range configDefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString("Configuration:\n")
	hadAny := false
	for _, key := range keys {
		value, err := bot.getConfig(ctx, key)
		if err != nil {
			return configErrorResult(err, "read")
		}
		if err := bot.Check(ctx, actor, value.Def.ReadPermission); err != nil {
			if errors.Is(err, command.ErrDenied) {
				continue
			}
			return configErrorResult(err, "read")
		}
		hadAny = true
		builder.WriteString("- `")
		builder.WriteString(value.Def.Key)
		builder.WriteString("` = `")
		builder.WriteString(redactConfig(value))
		builder.WriteString("`")
		if value.IsDefault {
			builder.WriteString(" (default)")
		}
		builder.WriteByte('\n')
	}
	if !hadAny {
		return command.Result{Content: "No configuration values are visible to you."}
	}
	return command.Result{Content: strings.TrimSpace(builder.String())}
}

func (bot *Bot) configGet(ctx context.Context, actor command.Actor, key string) command.Result {
	value, err := bot.getConfig(ctx, key)
	if err != nil {
		return configErrorResult(err, "read")
	}
	if err := bot.Check(ctx, actor, value.Def.ReadPermission); err != nil {
		return configErrorResult(err, "read")
	}
	return command.Result{Content: fmt.Sprintf("`%s` = `%s`", value.Def.Key, redactConfig(value))}
}

func (bot *Bot) configSet(
	ctx context.Context,
	actor command.Actor,
	messageID int64,
	key, value string,
) command.Result {
	def, ok := configDefs[key]
	if !ok {
		return configErrorResult(fmt.Errorf("%w: %s", errUnknownConfigKey, key), "change")
	}
	if err := bot.Check(ctx, actor, def.WritePermission); err != nil {
		return configErrorResult(err, "change")
	}
	_, newValue, err := bot.setConfig(ctx, actor, messageID, key, value)
	if err != nil {
		return configErrorResult(err, "change")
	}
	return command.Result{
		Content: fmt.Sprintf("Configuration updated: `%s` = `%s`", newValue.Def.Key, redactConfig(newValue)),
	}
}

func configErrorResult(err error, action string) command.Result {
	if errors.Is(err, errUnknownConfigKey) {
		return command.Result{Content: "Unknown configuration key."}
	}
	if errors.Is(err, command.ErrDenied) {
		return command.Result{Content: fmt.Sprintf("You are not authorized to %s that configuration value.", action)}
	}
	if errors.Is(err, command.ErrPermissionUnavailable) {
		return command.Result{Content: "I cannot verify permissions right now, so I will not access configuration."}
	}
	var userErr command.UserError
	if errors.As(err, &userErr) {
		return command.Result{Content: userErr.Message}
	}
	return command.Result{Content: "Configuration error: " + err.Error()}
}

func validateConfigBool(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return "true", nil
	case "false", "0", "no", "off":
		return "false", nil
	default:
		return "", errors.New("expected a boolean value")
	}
}

func validateConfigPositiveInt64(value string) (string, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || v <= 0 {
		return "", errors.New("expected a positive integer")
	}
	return strconv.FormatInt(v, 10), nil
}

func validateConfigNonEmptyString(value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", errors.New("value must not be empty")
	}
	return v, nil
}
