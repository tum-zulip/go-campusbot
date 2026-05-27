//nolint:dupl,noctx,testpackage // Tests share cache internals and mirrored scenarios with user-groups cache.
package zulipcache

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

func TestStreamsTransportCachesFreshRequests(t *testing.T) {
	var calls atomic.Int64
	cache := NewStreams(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"result":"success","msg":"","streams":[{"stream_id":1,"name":"one"}]}`), nil
	}))}

	for range 2 {
		resp, err := client.Get("https://zulip.example/api/v1/streams?include_all=true")
		if err != nil {
			t.Fatalf("GET streams: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if !strings.Contains(string(body), `"name":"one"`) {
			t.Fatalf("response body = %s", body)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("base calls = %d, want 1", got)
	}
}

func TestStreamsHandleEventUpdatesCachedBody(t *testing.T) {
	cache := NewStreams(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"result":"success","msg":"","streams":[{"stream_id":1,"name":"one"}]}`), nil
	}))}
	if _, err := client.Get("https://zulip.example/api/v1/streams?include_all=true"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	cache.HandleEvent(events.ChannelCreateEvent{
		Channels: []zulip.Channel{{ChannelID: 2, Name: "two"}},
	})
	cache.HandleEvent(events.ChannelDeleteEvent{ChannelIDs: []int64{1}})

	resp, err := client.Get("https://zulip.example/api/v1/streams?include_all=true")
	if err != nil {
		t.Fatalf("GET cached streams: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if strings.Contains(string(body), `"stream_id":1`) {
		t.Fatalf("removed stream still present: %s", body)
	}
	if !strings.Contains(string(body), `"name":"two"`) {
		t.Fatalf("added stream missing: %s", body)
	}
}

func TestStreamsHandleUnarchiveEventDoesNotInvalidate(t *testing.T) {
	cache := NewStreams(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(
			`{"result":"success","msg":"","streams":[{"stream_id":1,"name":"one","is_archived":true}]}`,
		), nil
	}))}
	if _, err := client.Get("https://zulip.example/api/v1/streams?include_all=true"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	archived := false
	cache.HandleEvent(events.ChannelUpdateEvent{
		ChannelID: 1,
		Name:      "one",
		Property:  "is_archived",
		Value:     &events.ChannelEventUpdateValue{Bool: &archived},
	})

	resp, err := client.Get("https://zulip.example/api/v1/streams?include_all=true")
	if err != nil {
		t.Fatalf("GET cached streams: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(body), `"stream_id":1`) {
		t.Fatalf("stream missing after unarchive event: %s", body)
	}
	if strings.Contains(string(body), `"is_archived":true`) {
		t.Fatalf("stream still archived after unarchive event: %s", body)
	}
}
