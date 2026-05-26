package command

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type Invocation struct {
	Name    string
	Args    []string
	RawArgs string
}

// Parse parses content as a command invocation.
// The entire message content is treated as the command — no prefix is required.
// Empty or whitespace-only content returns ErrNotCommand.
// Content with an invalid first token returns ErrMalformed.
func Parse(content string) (Invocation, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return Invocation{}, ErrNotCommand
	}

	parts, err := splitArgs(trimmed)
	if err != nil {
		return Invocation{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if len(parts) == 0 {
		return Invocation{}, ErrNotCommand
	}

	name := strings.ToLower(parts[0])
	if !validCommandName(name) {
		return Invocation{}, fmt.Errorf("%w: invalid command name %q", ErrMalformed, parts[0])
	}

	rawArgs := strings.TrimSpace(strings.TrimPrefix(trimmed, parts[0]))
	return Invocation{
		Name:    name,
		Args:    parts[1:],
		RawArgs: rawArgs,
	}, nil
}

func validCommandName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '-' || r == '_') {
			continue
		}
		return false
	}
	return true
}

func splitArgs(value string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	escaped := false
	inToken := false

	flush := func() {
		args = append(args, current.String())
		current.Reset()
		inToken = false
	}

	for _, r := range value {
		if escaped {
			current.WriteRune(r)
			escaped = false
			inToken = true
			continue
		}
		if r == '\\' {
			escaped = true
			inToken = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
			inToken = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			inToken = true
			continue
		}
		if unicode.IsSpace(r) {
			if inToken {
				flush()
			}
			continue
		}
		current.WriteRune(r)
		inToken = true
	}

	if escaped {
		return nil, errors.New("trailing escape character")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quoted argument")
	}
	if inToken {
		flush()
	}
	return args, nil
}
