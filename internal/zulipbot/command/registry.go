package command

import (
	"context"
	"fmt"
	"sort"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
)

type Metadata struct {
	Name       string
	Summary    string
	Usage      string
	// OwnerUsage, when non-empty, overrides Usage in help output shown to owners.
	// Use this for commands that expose additional subcommands only owners may run.
	OwnerUsage string
	Permission permissions.Permission
	Privileged bool
}

type Request struct {
	Invocation Invocation
	Actor      model.Actor
	MessageID  int64
	Target     model.ReplyTarget
}

type Result struct {
	Content       string
	AfterResponse func(context.Context) error
}

type Handler interface {
	Metadata() Metadata
	Handle(ctx context.Context, req Request) (Result, error)
}

type HandlerFunc struct {
	Meta Metadata
	Fn   func(ctx context.Context, req Request) (Result, error)
}

func (handler HandlerFunc) Metadata() Metadata {
	return handler.Meta
}

func (handler HandlerFunc) Handle(ctx context.Context, req Request) (Result, error) {
	return handler.Fn(ctx, req)
}

type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

func (registry *Registry) Register(handler Handler) error {
	if handler == nil {
		return fmt.Errorf("command handler must not be nil")
	}
	meta := handler.Metadata()
	if !validCommandName(meta.Name) {
		return fmt.Errorf("invalid command name %q", meta.Name)
	}
	if meta.Permission == "" {
		return fmt.Errorf("command %q has no permission", meta.Name)
	}
	if _, exists := registry.handlers[meta.Name]; exists {
		return fmt.Errorf("command %q already registered", meta.Name)
	}
	registry.handlers[meta.Name] = handler
	return nil
}

func (registry *Registry) Lookup(name string) (Handler, bool) {
	handler, ok := registry.handlers[name]
	return handler, ok
}

func (registry *Registry) Metadata() []Metadata {
	items := make([]Metadata, 0, len(registry.handlers))
	for _, handler := range registry.handlers {
		items = append(items, handler.Metadata())
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})
	return items
}
