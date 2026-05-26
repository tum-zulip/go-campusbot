package command

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/tum-zulip/go-zulip/zulip"
)

// UserResolver resolves a Zulip user ID to the full user record.
type UserResolver interface {
	GetUserByID(ctx context.Context, id int64) (zulip.User, error)
}

// ArgParser parses raw argument tokens into typed structs according to an ArgSpec.
type ArgParser struct {
	resolver UserResolver
}

// NewArgParser creates an ArgParser. resolver may be nil if no command uses
// zulip.User-typed fields.
func NewArgParser(resolver UserResolver) *ArgParser {
	return &ArgParser{resolver: resolver}
}

// Parse walks spec against rawArgs and returns a populated value.
//
// spec must be one of:
//   - SubcmdSpec  – consumes one token as the subcommand key and recurses
//   - a struct value – fills fields by reflection from remaining tokens
//   - nil – expects no tokens; returns NoArgs{}
//
// All validation errors are returned as UserError so the router can forward
// them directly to the user.
func (p *ArgParser) Parse(ctx context.Context, spec any, rawArgs []string) (any, error) {
	if spec == nil {
		if len(rawArgs) > 0 {
			return nil, NewUserError(fmt.Sprintf("unexpected argument(s): %q", strings.Join(rawArgs, " ")))
		}
		return NoArgs{}, nil
	}

	if sub, ok := spec.(SubcmdSpec); ok {
		return p.parseSubcmd(ctx, sub, rawArgs)
	}

	return p.parseStruct(ctx, spec, rawArgs)
}

func (p *ArgParser) parseSubcmd(ctx context.Context, spec SubcmdSpec, rawArgs []string) (any, error) {
	if len(rawArgs) == 0 {
		nested, ok := spec[""]
		if ok {
			return p.Parse(ctx, nested, nil)
		}
		return nil, NewUserError(fmt.Sprintf("expected subcommand: %s", joinKeys(spec)))
	}
	nested, ok := spec[rawArgs[0]]
	if !ok {
		return nil, NewUserError(fmt.Sprintf("unknown subcommand %q, expected one of: %s", rawArgs[0], joinKeys(spec)))
	}
	return p.Parse(ctx, nested, rawArgs[1:])
}

var zulipUserType = reflect.TypeOf(zulip.User{}) //nolint:gochecknoglobals // cached reflect.Type

func (p *ArgParser) parseStruct(ctx context.Context, spec any, rawArgs []string) (any, error) {
	t := reflect.TypeOf(spec)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("argparser: spec must be a struct or SubcmdSpec, got %T", spec)
	}

	v := reflect.New(t).Elem()
	remaining := p.extractFlags(t, v, rawArgs)

	posIdx, err := p.fillPositional(ctx, t, v, remaining)
	if err != nil {
		return nil, err
	}

	if posIdx < len(remaining) {
		extra := strings.Join(remaining[posIdx:], " ")
		return nil, NewUserError(fmt.Sprintf("unexpected argument(s): %q (usage: %s)", extra, buildUsageHint(t)))
	}

	return v.Interface(), nil
}

func (p *ArgParser) extractFlags(t reflect.Type, v reflect.Value, rawArgs []string) []string {
	remaining := slices.Clone(rawArgs)
	for i := range t.NumField() {
		field := t.Field(i)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		flagName := field.Tag.Get("arg")
		if flagName == "" || !strings.HasPrefix(flagName, "-") {
			continue
		}
		found := false
		filtered := remaining[:0:len(remaining)]
		filtered = filtered[:0]
		for _, arg := range remaining {
			if arg == flagName && !found {
				found = true
			} else {
				filtered = append(filtered, arg)
			}
		}
		remaining = filtered
		if found {
			v.Field(i).SetBool(true)
		}
	}
	return remaining
}

func (p *ArgParser) fillPositional(
	ctx context.Context,
	t reflect.Type,
	v reflect.Value,
	remaining []string,
) (int, error) {
	posIdx := 0
	for i := range t.NumField() {
		field := t.Field(i)
		if field.Type.Kind() == reflect.Bool {
			continue
		}
		optional := field.Tag.Get("optional") == "true"
		if posIdx >= len(remaining) {
			if optional {
				continue
			}
			return 0, NewUserError("Usage: " + buildUsageHint(t))
		}
		token := remaining[posIdx]
		posIdx++
		if err := p.setField(ctx, v.Field(i), field, token); err != nil {
			return 0, err
		}
	}
	return posIdx, nil
}

func (p *ArgParser) setField(ctx context.Context, fv reflect.Value, field reflect.StructField, token string) error {
	argName := field.Tag.Get("arg")
	if argName == "" {
		argName = camelToSnake(field.Name)
	}

	switch {
	case field.Type.Kind() == reflect.String:
		fv.SetString(token)

	case field.Type.Kind() == reflect.Int64:
		n, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return NewUserError(fmt.Sprintf("%s must be an integer, got %q", argName, token))
		}
		fv.SetInt(n)

	case field.Type == zulipUserType:
		n, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return NewUserError(fmt.Sprintf("%s must be a user ID (integer), got %q", argName, token))
		}
		if p.resolver == nil {
			return fmt.Errorf("argparser: UserResolver required for zulip.User field %q but not configured", field.Name)
		}
		user, err := p.resolver.GetUserByID(ctx, n)
		if err != nil {
			return fmt.Errorf("resolve user %d: %w", n, err)
		}
		fv.Set(reflect.ValueOf(user))

	default:
		return fmt.Errorf("argparser: unsupported field type %v for field %q", field.Type, field.Name)
	}
	return nil
}

func buildUsageHint(t reflect.Type) string {
	parts := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		field := t.Field(i)
		name := field.Tag.Get("arg")
		if name == "" {
			name = camelToSnake(field.Name)
		}
		switch {
		case field.Type.Kind() == reflect.Bool:
			parts = append(parts, "["+name+"]")
		case field.Tag.Get("optional") == "true":
			parts = append(parts, "["+name+"]")
		default:
			parts = append(parts, "<"+name+">")
		}
	}
	return strings.Join(parts, " ")
}

func joinKeys(spec SubcmdSpec) string {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
