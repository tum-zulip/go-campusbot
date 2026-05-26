package command

import "errors"

var (
	ErrNotCommand = errors.New("message is not a bot command")
	ErrMalformed  = errors.New("malformed command")
)

type UserError struct {
	Message string
}

func (err UserError) Error() string {
	return err.Message
}

func NewUserError(message string) UserError {
	return UserError{Message: message}
}
