package zulipbot

import (
	"context"
	"sync"
)

type Provider struct {
	mu  sync.Mutex
	bot *Bot
	cfg Config
}

func NewProvider(cfg Config) *Provider {
	return &Provider{
		cfg: cfg.withDefaults(),
	}
}

func (provider *Provider) Bot(ctx context.Context) (*Bot, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	if provider.bot != nil {
		return provider.bot, nil
	}

	bot, err := New(ctx, provider.cfg)
	if err != nil {
		return nil, err
	}

	provider.bot = bot
	return provider.bot, nil
}

func (provider *Provider) Initialized() bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	return provider.bot != nil
}
