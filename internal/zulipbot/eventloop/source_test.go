package eventloop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	z "github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
)

func TestZulipSourceRegisterSendsBroadQueueCapabilities(t *testing.T) {
	t.Parallel()

	var registerForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/register" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want %s", r.Method, http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		registerForm = r.PostForm

		var capabilities map[string]bool
		if err := json.Unmarshal([]byte(r.PostForm.Get("client_capabilities")), &capabilities); err != nil {
			t.Errorf("client_capabilities is not JSON object: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !capabilities["notification_settings_null"] {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(
				t,
				w,
				`{"result":"error","code":"BAD_REQUEST","msg":"client_capabilities[\"notification_settings_null\"] field is missing: Field required"}`,
			)
			return
		}

		writeJSON(t, w, `{"result":"success","queue_id":"queue-1","last_event_id":42}`)
	}))
	t.Cleanup(server.Close)

	client, err := zulipclient.NewClient(
		&z.RC{Email: "bot@example.com", APIKey: "test-api-key", Site: server.URL},
		zulipclient.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		zulipclient.WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("NewClient() failed: %v", err)
	}
	source := NewZulipSource(client)

	state, err := source.Register(context.Background(), RegisterOptions{})
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	if state.QueueID != "queue-1" || state.LastEventID != 42 {
		t.Fatalf("state = %#v, want queue-1/42", state)
	}
	if registerForm == nil {
		t.Fatal("Zulip register endpoint was not called")
	}

	var capabilities map[string]bool
	if err := json.Unmarshal([]byte(registerForm.Get("client_capabilities")), &capabilities); err != nil {
		t.Fatalf("client_capabilities is not JSON object: %v", err)
	}
	for key, want := range map[string]bool{
		"empty_topic_name":           true,
		"notification_settings_null": true,
		"user_settings_object":       true,
	} {
		if got := capabilities[key]; got != want {
			t.Fatalf("client_capabilities[%q] = %t, want %t; full capabilities=%v", key, got, want, capabilities)
		}
	}

	if got := registerForm.Get("all_public_streams"); got != "true" {
		t.Fatalf("all_public_streams = %q, want true; form=%v", got, registerForm)
	}
	for _, key := range []string{"event_types", "fetch_event_types", "narrow"} {
		if _, ok := registerForm[key]; ok {
			t.Fatalf("%s should be omitted; got %q in form %v", key, registerForm.Get(key), registerForm)
		}
	}
}

func TestZulipSourceRegisterToleratesUnknownResponseFields(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		body            string
		wantQueueID     string
		wantLastEventID int64
	}{
		{
			name: "new Zulip permission field in user groups",
			body: `{
				"result":"success",
				"queue_id":"queue-unknown",
				"last_event_id":42,
				"realm_user_groups":[{
					"id":1,
					"name":"Admins",
					"description":"",
					"members":[],
					"direct_subgroup_ids":[],
					"is_system_group":true,
					"can_create_topic_group":{"direct_members":[],"direct_subgroups":[]}
				}]
			}`,
			wantQueueID:     "queue-unknown",
			wantLastEventID: 42,
		},
		{
			name: "arbitrary future response field",
			body: `{
				"result":"success",
				"queue_id":"queue-future",
				"last_event_id":43,
				"future_register_field":{"nested":true}
			}`,
			wantQueueID:     "queue-future",
			wantLastEventID: 43,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := newRegisterTestSource(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, tc.body)
			})

			state, err := source.Register(context.Background(), RegisterOptions{})
			if err != nil {
				t.Fatalf("Register() failed: %v", err)
			}
			if state.QueueID != tc.wantQueueID || state.LastEventID != tc.wantLastEventID {
				t.Fatalf(
					"state = %#v, want queue ID %q and last event ID %d",
					state,
					tc.wantQueueID,
					tc.wantLastEventID,
				)
			}
		})
	}
}

func TestZulipSourceRegisterSurfacesBadRequestErrors(t *testing.T) {
	t.Parallel()

	source := newRegisterTestSource(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(t, w, `{"result":"error","code":"BAD_REQUEST","msg":"bad register request"}`)
	})

	_, err := source.Register(context.Background(), RegisterOptions{})
	if err == nil {
		t.Fatal("Register() succeeded, want BAD_REQUEST error")
	}
	if got := err.Error(); !strings.Contains(got, "BAD_REQUEST") || !strings.Contains(got, "bad register request") {
		t.Fatalf("error = %q, want BAD_REQUEST message", got)
	}
}

func newRegisterTestSource(t *testing.T, handleRegister func(http.ResponseWriter, *http.Request)) *ZulipSource {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/register" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want %s", r.Method, http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		handleRegister(w, r)
	}))
	t.Cleanup(server.Close)

	client, err := zulipclient.NewClient(
		&z.RC{Email: "bot@example.com", APIKey: "test-api-key", Site: server.URL},
		zulipclient.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		zulipclient.WithMaxRetries(0),
	)
	if err != nil {
		t.Fatalf("NewClient() failed: %v", err)
	}
	return NewZulipSource(client)
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}
