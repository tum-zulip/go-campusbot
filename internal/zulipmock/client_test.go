package zulipmock

import (
	"context"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip/client"
)

func TestClientImplementsUpstreamClient(t *testing.T) {
	var _ client.Client = NewClient()
}

func TestBuilderExecuteReturnsNil(t *testing.T) {
	resp, httpResp, err := NewClient().SendMessage(context.Background()).Execute()
	if resp != nil {
		t.Fatalf("response = %#v, want nil", resp)
	}
	if httpResp != nil {
		t.Fatalf("http response = %#v, want nil", httpResp)
	}
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
}
