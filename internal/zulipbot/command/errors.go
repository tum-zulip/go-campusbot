package command

import "errors"

// ErrNotCommand is returned when a message is not addressed to the bot as a command.
var ErrNotCommand = errors.New("message is not a bot command")

// ErrMalformed is returned when a command cannot be parsed.
var ErrMalformed = errors.New("malformed command")

// ErrDenied is returned when the actor lacks the required role.
var ErrDenied = errors.New("permission denied")

// ErrPermissionUnavailable is returned when the role lookup fails and the action cannot proceed.
var ErrPermissionUnavailable = errors.New("permission unavailable")

type UserError struct {
	Message string
}

func (err UserError) Error() string {
	return err.Message
}

func NewUserError(message string) UserError {
	return UserError{Message: message}
}
