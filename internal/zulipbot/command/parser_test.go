package command

import (
	"errors"
	"testing"
)

func TestParserParsesDirectMessageCommand(t *testing.T) {
	t.Parallel()

	invocation, err := Parser{}.Parse(`config set command_prefix "!bot"`)
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

func TestParserHandlesLeadingAndTrailingWhitespace(t *testing.T) {
	t.Parallel()

	invocation, err := Parser{}.Parse("  restart  ")
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
			_, err := Parser{}.Parse(input)
			if !errors.Is(err, ErrNotCommand) {
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
			_, err := Parser{}.Parse(tt.input)
			if !errors.Is(err, ErrMalformed) {
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
			invocation, err := Parser{}.Parse(tt.input)
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
	invocation, err := Parser{}.Parse("hello world")
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}
	if invocation.Name != "hello" {
		t.Fatalf("Name = %q, want hello", invocation.Name)
	}
}
