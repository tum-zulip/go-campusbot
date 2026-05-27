package command_test

import (
	"errors"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

func TestParserParsesDirectMessageCommand(t *testing.T) {
	t.Parallel()

	invocation, err := command.Parse(`config set command_prefix "!bot"`)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if invocation.Name != "config" {
		t.Fatalf("Name = %q, want config", invocation.Name)
	}
	wantArgs := []string{"set", "command_prefix", "!bot"}
	if len(invocation.Args) != len(wantArgs) {
		t.Fatalf("Args = %#v, want %#v", invocation.Args, wantArgs)
	}
	for i := range wantArgs {
		if invocation.Args[i] != wantArgs[i] {
			t.Fatalf("Args[%d] = %q, want %q", i, invocation.Args[i], wantArgs[i])
		}
	}
}

func TestParserSplitsQuotedArgumentWithSpaces(t *testing.T) {
	t.Parallel()

	invocation, err := command.Parse(`group channel create "The New Channel Name" WI`)
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	wantArgs := []string{"channel", "create", "The New Channel Name", "WI"}
	assertArgs(t, invocation.Args, wantArgs)
}

func TestParserKeepsZulipMentionsWithSpacesAsSingleArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		wantArgs []string
	}{
		{
			name:     "user mention",
			input:    `role set @**The User Name** admin`,
			wantArgs: []string{"set", "@**The User Name**", "admin"},
		},
		{
			name:     "silent user mention",
			input:    `role set @_**The User Name** admin`,
			wantArgs: []string{"set", "@_**The User Name**", "admin"},
		},
		{
			name:     "channel mention",
			input:    `group channel add #**The Channel Name** WI`,
			wantArgs: []string{"channel", "add", "#**The Channel Name**", "WI"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			invocation, err := command.Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() failed: %v", err)
			}
			assertArgs(t, invocation.Args, tt.wantArgs)
		})
	}
}

func TestParserHandlesLeadingAndTrailingWhitespace(t *testing.T) {
	t.Parallel()

	invocation, err := command.Parse("  restart  ")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if invocation.Name != "restart" {
		t.Fatalf("Name = %q, want restart", invocation.Name)
	}
	if len(invocation.Args) != 0 {
		t.Fatalf("Args = %#v, want []", invocation.Args)
	}
}

func assertArgs(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Args = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParserIgnoresEmptyAndWhitespaceOnlyMessages(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"   ",
		"\t\n",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			_, err := command.Parse(input)
			if !errors.Is(err, command.ErrNotCommand) {
				t.Fatalf("Parse(%q) error = %v, want ErrNotCommand", input, err)
			}
		})
	}
}

func TestParserRejectsMalformedCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "unterminated quote", input: `config "unterminated`},
		{name: "starts with bang (old prefix)", input: "!campusbot restart"},
		{name: "starts with dash", input: "-bad"},
		{name: "starts with at-sign", input: "@bot restart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := command.Parse(tt.input)
			if !errors.Is(err, command.ErrMalformed) {
				t.Fatalf("Parse(%q) error = %v, want ErrMalformed", tt.input, err)
			}
		})
	}
}

func TestParserParsesMultiWordCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		wantName string
		wantArgs []string
	}{
		{input: "help", wantName: "help", wantArgs: nil},
		{input: "status", wantName: "status", wantArgs: nil},
		{input: "config list", wantName: "config", wantArgs: []string{"list"}},
		{input: "config get some.key", wantName: "config", wantArgs: []string{"get", "some.key"}},
		{input: "role set 12345 admin", wantName: "role", wantArgs: []string{"set", "12345", "admin"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			invocation, err := command.Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", tt.input, err)
			}
			if invocation.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", invocation.Name, tt.wantName)
			}
			if len(invocation.Args) != len(tt.wantArgs) {
				t.Fatalf("Args = %#v, want %#v", invocation.Args, tt.wantArgs)
			}
			for i := range tt.wantArgs {
				if invocation.Args[i] != tt.wantArgs[i] {
					t.Fatalf("Args[%d] = %q, want %q", i, invocation.Args[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestParserUnknownCommandNameIsStillParsed(t *testing.T) {
	t.Parallel()

	// Unknown commands (e.g., 'hello') are parsed successfully; the router handles them.
	invocation, err := command.Parse("hello world")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if invocation.Name != "hello" {
		t.Fatalf("Name = %q, want hello", invocation.Name)
	}
}

func TestParserParsesCommandChains(t *testing.T) {
	t.Parallel()

	chain, err := command.ParseChain(`status && bogus || help ; config get "some;key"`)
	if err != nil {
		t.Fatalf("ParseChain() failed: %v", err)
	}
	if len(chain.Segments) != 4 {
		t.Fatalf("Segments = %#v, want 4", chain.Segments)
	}

	want := []struct {
		operator command.ChainOperator
		name     string
		args     []string
	}{
		{operator: command.ChainAlways, name: "status"},
		{operator: command.ChainAnd, name: "bogus"},
		{operator: command.ChainOr, name: "help"},
		{operator: command.ChainThen, name: "config", args: []string{"get", "some;key"}},
	}
	for i, wantSegment := range want {
		got := chain.Segments[i]
		if got.Operator != wantSegment.operator {
			t.Fatalf("Segments[%d].Operator = %q, want %q", i, got.Operator, wantSegment.operator)
		}
		if got.Invocation.Name != wantSegment.name {
			t.Fatalf("Segments[%d].Name = %q, want %q", i, got.Invocation.Name, wantSegment.name)
		}
		assertArgs(t, got.Invocation.Args, wantSegment.args)
	}
}

func TestParserRejectsMalformedCommandChains(t *testing.T) {
	t.Parallel()

	tests := []string{
		"status &&",
		"status ; ; help",
		"status || && help",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			_, err := command.ParseChain(input)
			if !errors.Is(err, command.ErrMalformed) {
				t.Fatalf("ParseChain(%q) error = %v, want ErrMalformed", input, err)
			}
		})
	}
}
