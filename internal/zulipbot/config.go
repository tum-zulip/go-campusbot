package zulipbot

import (
	"io"
	"log/slog"
)

const (
	DefaultClientName = "go-campusbot"
	DefaultRCPath     = "zuliprc"
)

type Config struct {
	RCPath     string
	ClientName string
	Logger     *slog.Logger
}

func (cfg Config) withDefaults() Config {
	if cfg.RCPath == "" {
		cfg.RCPath = DefaultRCPath
	}
	if cfg.ClientName == "" {
		cfg.ClientName = DefaultClientName
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return cfg
}
