//nolint:dupl,noctx,testpackage // Tests share cache internals and mirrored scenarios with streams cache.
package zulipcache

import (
	"context"
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

	firstResp, firstErr := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false")
	if firstErr != nil {
		t.Fatalf("first GET user groups: %v", firstErr)
	}
	if err := firstResp.Body.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}
	time.Sleep(time.Millisecond)
	secondResp, secondErr := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false")
	if secondErr != nil {
		t.Fatalf("second GET user groups: %v", secondErr)
	}
	if err := secondResp.Body.Close(); err != nil {
		t.Fatalf("close second response: %v", err)
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
	primeResp, primeErr := client.Get("https://zulip.example/api/v1/user_groups?include_deactivated_groups=false")
	if primeErr != nil {
		t.Fatalf("prime cache: %v", primeErr)
	}
	if err := primeResp.Body.Close(); err != nil {
		t.Fatalf("close prime response: %v", err)
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

func TestUserGroupsTransportServesBothVariantsFromOneFetch(t *testing.T) {
	var calls atomic.Int64
	cache := NewUserGroups(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if got := req.URL.Query().Get("include_deactivated_groups"); got != "true" {
			t.Errorf("upstream include_deactivated_groups = %q, want true", got)
		}
		return jsonResponse(`{"result":"success","msg":"","user_groups":[` +
			`{"id":1,"name":"active"},{"id":2,"name":"archived","deactivated":true}]}`), nil
	}))}

	for _, query := range []string{"", "?include_deactivated_groups=false"} {
		body := getBody(t, client, "https://zulip.example/api/v1/user_groups"+query)
		if !strings.Contains(body, `"name":"active"`) || strings.Contains(body, `"name":"archived"`) {
			t.Errorf("active listing %q = %s, want only the active group", query, body)
		}
	}
	body := getBody(t, client, "https://zulip.example/api/v1/user_groups?include_deactivated_groups=true")
	if !strings.Contains(body, `"name":"active"`) || !strings.Contains(body, `"name":"archived"`) {
		t.Errorf("full listing = %s, want both groups", body)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("base calls = %d, want 1", got)
	}
}

func TestUserGroupsHandleEventTracksDeactivation(t *testing.T) {
	cache := NewUserGroups(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"result":"success","msg":"","user_groups":[{"id":1,"name":"one"}]}`), nil
	}))}
	const (
		activeURL = "https://zulip.example/api/v1/user_groups"
		allURL    = "https://zulip.example/api/v1/user_groups?include_deactivated_groups=true"
	)
	getBody(t, client, activeURL)

	deactivated := true
	cache.HandleEvent(events.UserGroupUpdateEvent{
		GroupID: 1,
		Data:    events.UserGroupUpdateData{Deactivated: &deactivated},
	})
	if body := getBody(t, client, activeURL); strings.Contains(body, `"id":1`) {
		t.Errorf("active listing after deactivation = %s, want group 1 left out", body)
	}
	if body := getBody(t, client, allURL); !strings.Contains(body, `"deactivated":true`) {
		t.Errorf("full listing after deactivation = %s, want group 1 marked deactivated", body)
	}

	deactivated = false
	cache.HandleEvent(events.UserGroupUpdateEvent{
		GroupID: 1,
		Data:    events.UserGroupUpdateData{Deactivated: &deactivated},
	})
	if body := getBody(t, client, activeURL); !strings.Contains(body, `"id":1`) {
		t.Errorf("active listing after reactivation = %s, want group 1", body)
	}
}

func TestUserGroupsTransportWithoutCacheReachesZulip(t *testing.T) {
	var calls atomic.Int64
	cache := NewUserGroups(time.Minute)
	client := &http.Client{Transport: cache.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(`{"result":"success","msg":"","user_groups":[]}`), nil
	}))}
	const url = "https://zulip.example/api/v1/user_groups?include_deactivated_groups=true"
	getBody(t, client, url)

	req, err := http.NewRequestWithContext(WithoutCache(context.Background()), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET user groups without cache: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("base calls = %d, want 2", got)
	}
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return string(body)
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
