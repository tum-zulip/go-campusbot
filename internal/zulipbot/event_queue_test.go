//nolint:testpackage // Tests exercise unexported event-queue error classification.
package zulipbot

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"
)

func TestIsRecoverableEventQueueErrorAcceptsPrunedEventError(t *testing.T) {
	t.Parallel()

	err := zulip.CodedError{
		Response: zulip.Response{
			Result: zulip.ResponseError,
			Msg:    "An event newer than 99 has already been pruned!",
		},
		Code: "BAD_REQUEST",
	}

	if !isRecoverableEventQueueError(err) {
		t.Fatal("pruned event queue error should be recoverable")
	}
}

func TestIsRecoverableEventQueueErrorAcceptsWrappedPrunedAPIError(t *testing.T) {
	t.Parallel()

	body := []byte(`{"result":"error","msg":"An event newer than 99 has already been pruned!","code":"BAD_REQUEST"}`)
	err := fmt.Errorf("connect to Zulip event queue: %w", zulip.NewAPIError(body, errors.New("Bad Request")))

	if !isRecoverableEventQueueError(err) {
		t.Fatal("wrapped pruned API error should be recoverable")
	}
}

func TestIsRecoverableEventQueueErrorRejectsOtherBadRequests(t *testing.T) {
	t.Parallel()

	err := zulip.CodedError{
		Response: zulip.Response{
			Result: zulip.ResponseError,
			Msg:    "Invalid request",
		},
		Code: "BAD_REQUEST",
	}

	if isRecoverableEventQueueError(err) {
		t.Fatal("unrelated BAD_REQUEST should not be recoverable")
	}
}
