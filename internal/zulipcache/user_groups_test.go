//nolint:dupl,noctx,testpackage // Tests share cache internals and mirrored scenarios with streams cache.
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

func TestUserGroupsTransportCachesFreshFalseRequests(t *testing.T) {
	var calls atomic.Int64
	cache := NewUserGroups(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"result":"success","msg":"","user_groups":[{"id":1,"name":"one"}]}`), nil
	}))}

	for range 2 {
		resp, err := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false")
		if err != nil {
			t.Fatalf("GET user groups: %v", err)
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

func TestUserGroupsTransportClearsStaleCache(t *testing.T) {
	var calls atomic.Int64
	cache := NewUserGroups(time.Nanosecond)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"result":"success","msg":"","user_groups":[]}`), nil
	}))}

	if _, err := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false"); err != nil {
		t.Fatalf("first GET user groups: %v", err)
	}
	time.Sleep(time.Millisecond)
	if _, err := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false"); err != nil {
		t.Fatalf("second GET user groups: %v", err)
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("base calls = %d, want 2", got)
	}
}

func TestUserGroupsHandleEventUpdatesCachedBody(t *testing.T) {
	cache := NewUserGroups(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"result":"success","msg":"","user_groups":[{"id":1,"name":"one"}]}`), nil
	}))}
	if _, err := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	renamed := "renamed"
	cache.HandleEvent(events.UserGroupUpdateEvent{
		GroupID: 1,
		Data: events.UserGroupUpdateData{
			Name: &renamed,
		},
	})
	cache.HandleEvent(events.UserGroupAddEvent{Group: zulip.UserGroup{ID: 2, Name: "two"}})
	cache.HandleEvent(events.UserGroupRemoveEvent{GroupID: 1})

	resp, err := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false")
	if err != nil {
		t.Fatalf("GET cached user groups: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if strings.Contains(string(body), `"id":1`) {
		t.Fatalf("removed group still present: %s", body)
	}
	if !strings.Contains(string(body), `"name":"two"`) {
		t.Fatalf("added group missing: %s", body)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}
