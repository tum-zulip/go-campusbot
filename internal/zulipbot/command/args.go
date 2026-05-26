package command

// SubcmdSpec maps subcommand words to either a leaf arg struct (any non-nil
// struct value), a nested SubcmdSpec (for further dispatch), or nil (leaf
// with zero remaining args expected, mapped to NoArgs).
// An empty-string key "" handles the "no subcommand token given" case.
type SubcmdSpec map[string]any

// NoArgs is the result type for commands and subcommands that take no arguments.
type NoArgs struct{}
