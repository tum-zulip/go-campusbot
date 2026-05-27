package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

// --- helpers ---

func parse(t *testing.T, spec any, rawArgs []string) any {
	t.Helper()
	parser := command.NewArgParser(nil)
	result, err := parser.Parse(context.Background(), spec, rawArgs)
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}
	return result
}

func parseErr(t *testing.T, spec any, rawArgs []string) string {
	t.Helper()
	parser := command.NewArgParser(nil)
	_, err := parser.Parse(context.Background(), spec, rawArgs)
	if err == nil {
		t.Fatalf("Parse() expected error but got none")
	}
	return err.Error()
}

// --- nil spec (no-arg commands) ---

func TestArgParserNilSpecNoArgs(t *testing.T) {
	t.Parallel()
	result := parse(t, nil, nil)
	if _, ok := result.(command.NoArgs); !ok {
		t.Fatalf("expected NoArgs, got %T", result)
	}
}

func TestArgParserNilSpecRejectsExtraArgs(t *testing.T) {
	t.Parallel()
	msg := parseErr(t, nil, []string{"extra"})
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
}

// --- empty struct (NoArgs) ---

func TestArgParserNoArgsStructEmpty(t *testing.T) {
	t.Parallel()
	result := parse(t, command.NoArgs{}, nil)
	if _, ok := result.(command.NoArgs); !ok {
		t.Fatalf("expected NoArgs, got %T", result)
	}
}

func TestArgParserNoArgsStructRejectsExtra(t *testing.T) {
	t.Parallel()
	parseErr(t, command.NoArgs{}, []string{"unexpected"})
}

// --- string field ---

type strArgs struct {
	Name string `arg:"name" desc:"A name"`
}

func TestArgParserStringField(t *testing.T) {
	t.Parallel()
	result := parse(t, strArgs{}, []string{"hello"})
	got, ok := result.(strArgs)
	if !ok {
		t.Fatalf("expected strArgs, got %T", result)
	}
	if got.Name != "hello" {
		t.Fatalf("Name = %q, want %q", got.Name, "hello")
	}
}

func TestArgParserStringFieldMissingArg(t *testing.T) {
	t.Parallel()
	parseErr(t, strArgs{}, nil)
}

func TestArgParserStringFieldTooManyArgs(t *testing.T) {
	t.Parallel()
	parseErr(t, strArgs{}, []string{"hello", "extra"})
}

// --- int64 field ---

type intArgs struct {
	ID int64 `arg:"id" desc:"An integer ID"`
}

func TestArgParserInt64Field(t *testing.T) {
	t.Parallel()
	result := parse(t, intArgs{}, []string{"42"})
	got, ok := result.(intArgs)
	if !ok {
		t.Fatalf("expected intArgs, got %T", result)
	}
	if got.ID != 42 {
		t.Fatalf("ID = %d, want 42", got.ID)
	}
}

func TestArgParserInt64FieldInvalidInput(t *testing.T) {
	t.Parallel()
	parseErr(t, intArgs{}, []string{"notanumber"})
}

// --- bool flag ---

type flagArgs struct {
	Keep      bool   `arg:"-k"         desc:"Keep channels"`
	ShortName string `arg:"short_name" desc:"Course name"`
}

func TestArgParserFlagPresent(t *testing.T) {
	t.Parallel()
	result := parse(t, flagArgs{}, []string{"-k", "eist"})
	got, ok := result.(flagArgs)
	if !ok {
		t.Fatalf("expected flagArgs, got %T", result)
	}
	if !got.Keep {
		t.Fatal("Keep should be true")
	}
	if got.ShortName != "eist" {
		t.Fatalf("ShortName = %q, want %q", got.ShortName, "eist")
	}
}

func TestArgParserFlagPresentAfterPositional(t *testing.T) {
	t.Parallel()
	// flag can appear anywhere in token stream
	result := parse(t, flagArgs{}, []string{"eist", "-k"})
	got, ok := result.(flagArgs)
	if !ok {
		t.Fatalf("expected flagArgs, got %T", result)
	}
	if !got.Keep {
		t.Fatal("Keep should be true when flag appears after positional")
	}
	if got.ShortName != "eist" {
		t.Fatalf("ShortName = %q, want %q", got.ShortName, "eist")
	}
}

func TestArgParserFlagAbsent(t *testing.T) {
	t.Parallel()
	result := parse(t, flagArgs{}, []string{"eist"})
	got, ok := result.(flagArgs)
	if !ok {
		t.Fatalf("expected flagArgs, got %T", result)
	}
	if got.Keep {
		t.Fatal("Keep should be false when flag not present")
	}
}

// --- optional field ---

type optArgs struct {
	Required string `arg:"required"`
	Optional string `arg:"optional" optional:"true"`
}

func TestArgParserOptionalFieldProvided(t *testing.T) {
	t.Parallel()
	result := parse(t, optArgs{}, []string{"req", "opt"})
	got, ok := result.(optArgs)
	if !ok {
		t.Fatalf("expected optArgs, got %T", result)
	}
	if got.Required != "req" || got.Optional != "opt" {
		t.Fatalf("got Required=%q Optional=%q", got.Required, got.Optional)
	}
}

func TestArgParserOptionalFieldAbsent(t *testing.T) {
	t.Parallel()
	result := parse(t, optArgs{}, []string{"req"})
	got, ok := result.(optArgs)
	if !ok {
		t.Fatalf("expected optArgs, got %T", result)
	}
	if got.Required != "req" {
		t.Fatalf("Required = %q, want %q", got.Required, "req")
	}
	if got.Optional != "" {
		t.Fatalf("Optional = %q, want empty", got.Optional)
	}
}

// --- SubcmdSpec dispatch ---

type (
	subA struct{ Val string }
	subB struct{ Num int64 }
)

var testSpec = command.SubcmdSpec{
	"a": subA{},
	"b": subB{},
}

func TestArgParserSubcmdDispatchesA(t *testing.T) {
	t.Parallel()
	result := parse(t, testSpec, []string{"a", "hello"})
	got, ok := result.(subA)
	if !ok {
		t.Fatalf("expected subA, got %T", result)
	}
	if got.Val != "hello" {
		t.Fatalf("Val = %q, want %q", got.Val, "hello")
	}
}

func TestArgParserSubcmdDispatchesB(t *testing.T) {
	t.Parallel()
	result := parse(t, testSpec, []string{"b", "99"})
	got, ok := result.(subB)
	if !ok {
		t.Fatalf("expected subB, got %T", result)
	}
	if got.Num != 99 {
		t.Fatalf("Num = %d, want 99", got.Num)
	}
}

func TestArgParserSubcmdUnknownKey(t *testing.T) {
	t.Parallel()
	parseErr(t, testSpec, []string{"unknown"})
}

func TestArgParserSubcmdMissingSubcmd(t *testing.T) {
	t.Parallel()
	parseErr(t, testSpec, nil)
}

// --- nested SubcmdSpec ---

type innerArgs struct{ X string }

var nestedSpec = command.SubcmdSpec{
	"outer": command.SubcmdSpec{
		"inner": innerArgs{},
	},
}

func TestArgParserNestedSubcmd(t *testing.T) {
	t.Parallel()
	result := parse(t, nestedSpec, []string{"outer", "inner", "value"})
	got, ok := result.(innerArgs)
	if !ok {
		t.Fatalf("expected innerArgs, got %T", result)
	}
	if got.X != "value" {
		t.Fatalf("X = %q, want %q", got.X, "value")
	}
}

// --- empty-string key (default subcommand) ---

// defaultArgs represents the zero-arg default case (e.g. "group announce" with no subcommand).
type (
	defaultArgs struct{}
	namedArgs   struct{ N int64 }
)

var defaultSpec = command.SubcmdSpec{
	"":      defaultArgs{},
	"named": namedArgs{},
}

func TestArgParserDefaultSubcmdNoToken(t *testing.T) {
	t.Parallel()
	result := parse(t, defaultSpec, nil)
	if _, ok := result.(defaultArgs); !ok {
		t.Fatalf("expected defaultArgs, got %T", result)
	}
}

func TestArgParserDefaultSubcmdExplicitKey(t *testing.T) {
	t.Parallel()
	result := parse(t, defaultSpec, []string{"named", "7"})
	got, ok := result.(namedArgs)
	if !ok {
		t.Fatalf("expected namedArgs, got %T", result)
	}
	if got.N != 7 {
		t.Fatalf("N = %d, want 7", got.N)
	}
}

// --- zulip.User resolution ---

type userArgs struct {
	User zulip.User `arg:"user_id" desc:"Zulip user"`
}

type userMentionArgs struct {
	User zulip.User `arg:"user" mention_only:"true" desc:"Zulip user mention"`
}

type fakeResolver struct {
	users    map[int64]zulip.User
	channels map[int64]zulip.Channel
	rendered map[string]string
}

func (r *fakeResolver) GetUserByID(_ context.Context, id int64) (zulip.User, error) {
	u, ok := r.users[id]
	if !ok {
		return zulip.User{}, errors.New("user not found")
	}
	return u, nil
}

func (r *fakeResolver) GetChannelByID(_ context.Context, id int64) (zulip.Channel, error) {
	channel, ok := r.channels[id]
	if !ok {
		return zulip.Channel{}, errors.New("channel not found")
	}
	return channel, nil
}

func (r *fakeResolver) RenderMessage(_ context.Context, content string) (string, error) {
	rendered, ok := r.rendered[content]
	if !ok {
		return "", errors.New("rendered content not found")
	}
	return rendered, nil
}

func TestArgParserUserResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{users: map[int64]zulip.User{
		42: {UserID: 42, FullName: "Alice"},
	}}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), userArgs{}, []string{"42"})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got, ok := result.(userArgs)
	if !ok {
		t.Fatalf("expected userArgs, got %T", result)
	}
	if got.User.UserID != 42 || got.User.FullName != "Alice" {
		t.Fatalf("unexpected user: %+v", got.User)
	}
}

func TestArgParserUserMentionResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		users: map[int64]zulip.User{
			42: {UserID: 42, FullName: "The User Name"},
		},
		rendered: map[string]string{
			`@**The User Name**`: `<p><span data-user-id="42">@The User Name</span></p>`,
		},
	}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), userArgs{}, []string{`@**The User Name**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(userArgs)
	if got.User.UserID != 42 || got.User.FullName != "The User Name" {
		t.Fatalf("unexpected user: %+v", got.User)
	}
}

func TestArgParserUserMentionWithEmbeddedIDResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{users: map[int64]zulip.User{
		42: {UserID: 42, FullName: "The User Name"},
	}}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), userArgs{}, []string{`@**The User Name|42**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(userArgs)
	if got.User.UserID != 42 {
		t.Fatalf("UserID = %d, want 42", got.User.UserID)
	}
}

func TestArgParserUserMentionOnlyAcceptsMention(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{users: map[int64]zulip.User{
		42: {UserID: 42, FullName: "The User Name"},
	}}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), userMentionArgs{}, []string{`@**The User Name|42**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(userMentionArgs)
	if got.User.UserID != 42 {
		t.Fatalf("UserID = %d, want 42", got.User.UserID)
	}
}

func TestArgParserUserMentionOnlyRejectsInteger(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{users: map[int64]zulip.User{
		42: {UserID: 42, FullName: "The User Name"},
	}}
	parser := command.NewArgParser(resolver)
	_, err := parser.Parse(context.Background(), userMentionArgs{}, []string{"42"})
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Zulip user mention") {
		t.Fatalf("expected mention-only error, got %q", userErr.Message)
	}
}

func TestArgParserUserResolutionNotFound(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{users: map[int64]zulip.User{}}
	parser := command.NewArgParser(resolver)
	_, err := parser.Parse(context.Background(), userArgs{}, []string{"99"})
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestArgParserUserResolutionNonIntegerID(t *testing.T) {
	t.Parallel()
	parser := command.NewArgParser(nil)
	_, err := parser.Parse(context.Background(), userArgs{}, []string{"not-an-id"})
	if err == nil {
		t.Fatal("expected error for non-integer user ID")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error")
	}
}

// --- zulip.Channel resolution ---

type channelArgs struct {
	Channel zulip.Channel `arg:"channel" desc:"Zulip channel"`
}

type channelMentionArgs struct {
	Channel zulip.Channel `arg:"channel" mention_only:"true" desc:"Zulip channel mention"`
}

type channelIDArgs struct {
	ChannelID int64 `arg:"channel_id" desc:"Zulip channel ID"`
}

func TestArgParserChannelMentionResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		channels: map[int64]zulip.Channel{
			24: {ChannelID: 24, Name: "The Channel Name"},
		},
		rendered: map[string]string{
			`#**The Channel Name**`: `<p><a data-stream-id="24">#The Channel Name</a></p>`,
		},
	}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), channelArgs{}, []string{`#**The Channel Name**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(channelArgs)
	if got.Channel.ChannelID != 24 || got.Channel.Name != "The Channel Name" {
		t.Fatalf("unexpected channel: %+v", got.Channel)
	}
}

func TestArgParserChannelMentionOnlyAcceptsMention(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		channels: map[int64]zulip.Channel{
			24: {ChannelID: 24, Name: "The Channel Name"},
		},
		rendered: map[string]string{
			`#**The Channel Name**`: `<p><a data-stream-id="24">#The Channel Name</a></p>`,
		},
	}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), channelMentionArgs{}, []string{`#**The Channel Name**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(channelMentionArgs)
	if got.Channel.ChannelID != 24 || got.Channel.Name != "The Channel Name" {
		t.Fatalf("unexpected channel: %+v", got.Channel)
	}
}

func TestArgParserChannelMentionOnlyRejectsInteger(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{channels: map[int64]zulip.Channel{
		24: {ChannelID: 24, Name: "The Channel Name"},
	}}
	parser := command.NewArgParser(resolver)
	_, err := parser.Parse(context.Background(), channelMentionArgs{}, []string{"24"})
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Zulip channel mention") {
		t.Fatalf("expected mention-only error, got %q", userErr.Message)
	}
}

func TestArgParserChannelIDMentionResolution(t *testing.T) {
	t.Parallel()
	resolver := &fakeResolver{
		rendered: map[string]string{
			`#**The Channel Name**`: `<p><a data-stream-id="24">#The Channel Name</a></p>`,
		},
	}
	parser := command.NewArgParser(resolver)
	result, err := parser.Parse(context.Background(), channelIDArgs{}, []string{`#**The Channel Name**`})
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	got := result.(channelIDArgs)
	if got.ChannelID != 24 {
		t.Fatalf("ChannelID = %d, want 24", got.ChannelID)
	}
}

func TestArgParserChannelIDStillAcceptsInteger(t *testing.T) {
	t.Parallel()
	result := parse(t, channelIDArgs{}, []string{"24"})
	got := result.(channelIDArgs)
	if got.ChannelID != 24 {
		t.Fatalf("ChannelID = %d, want 24", got.ChannelID)
	}
}
