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

type ChainOperator string

const (
	ChainAlways ChainOperator = ""
	ChainAnd    ChainOperator = "&&"
	ChainOr     ChainOperator = "||"
	ChainThen   ChainOperator = ";"
)

type ChainSegment struct {
	Operator   ChainOperator
	Invocation Invocation
}

type Chain struct {
	Segments []ChainSegment
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

// ParseChain parses content as one or more command invocations connected by
// shell-style command chaining operators.
func ParseChain(content string) (Chain, error) {
	parts, err := splitChain(content)
	if err != nil {
		return Chain{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if len(parts) == 0 {
		return Chain{}, ErrNotCommand
	}

	segments := make([]ChainSegment, 0, len(parts))
	for i, part := range parts {
		invocation, err := Parse(part.content)
		if err != nil {
			return Chain{}, err
		}
		if i == 0 {
			part.operator = ChainAlways
		}
		segments = append(segments, ChainSegment{
			Operator:   part.operator,
			Invocation: invocation,
		})
	}
	return Chain{Segments: segments}, nil
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

type chainPart struct {
	operator ChainOperator
	content  string
}

//nolint:funlen,gocognit // mirrors splitArgs so quote and escape behavior stays local and explicit.
func splitChain(value string) ([]chainPart, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}

	var parts []chainPart
	var current strings.Builder
	operator := ChainAlways
	var quote rune
	escaped := false

	flush := func(next ChainOperator) error {
		content := strings.TrimSpace(current.String())
		if content == "" {
			return errors.New("empty command in chain")
		}
		parts = append(parts, chainPart{operator: operator, content: content})
		current.Reset()
		operator = next
		return nil
	}

	runes := []rune(trimmed)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			current.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			current.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			current.WriteRune(r)
			quote = r
			continue
		}
		switch {
		case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
			if err := flush(ChainAnd); err != nil {
				return nil, err
			}
			i++
		case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
			if err := flush(ChainOr); err != nil {
				return nil, err
			}
			i++
		case r == ';':
			if err := flush(ChainThen); err != nil {
				return nil, err
			}
		default:
			current.WriteRune(r)
		}
	}

	if escaped {
		return nil, errors.New("trailing escape character")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quoted argument")
	}
	if err := flush(ChainAlways); err != nil {
		return nil, err
	}
	return parts, nil
}

//nolint:funlen,gocognit // small shell-like scanner; splitting would spread parser state across helpers
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

	runes := []rune(value)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
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
		if !inToken {
			if token, next, ok := scanZulipMentionToken(runes, i); ok {
				current.WriteString(token)
				inToken = true
				i = next - 1
				continue
			}
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

func scanZulipMentionToken(runes []rune, start int) (string, int, bool) {
	switch runes[start] {
	case '@':
		i := start + 1
		if i < len(runes) && runes[i] == '_' {
			i++
		}
		if !hasRunesAt(runes, i, "**") {
			return "", start, false
		}
		end, ok := findRunes(runes, i+2, "**")
		if !ok {
			return "", start, false
		}
		return string(runes[start : end+2]), end + 2, true
	case '#':
		i := start + 1
		if !hasRunesAt(runes, i, "**") {
			return "", start, false
		}
		end, ok := findRunes(runes, i+2, "**")
		if !ok {
			return "", start, false
		}
		return string(runes[start : end+2]), end + 2, true
	default:
		return "", start, false
	}
}

func hasRunesAt(runes []rune, start int, value string) bool {
	needle := []rune(value)
	if start+len(needle) > len(runes) {
		return false
	}
	for i, r := range needle {
		if runes[start+i] != r {
			return false
		}
	}
	return true
}

func findRunes(runes []rune, start int, value string) (int, bool) {
	for i := start; i < len(runes); i++ {
		if hasRunesAt(runes, i, value) {
			return i, true
		}
	}
	return 0, false
}
