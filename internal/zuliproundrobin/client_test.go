package zuliproundrobin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
	"github.com/tum-zulip/go-campusbot/internal/zuliproundrobin"
	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
)

var _ zulipclient.Client = (*zuliproundrobin.Client)(nil)

func TestNewClientsRequiresClients(t *testing.T) {
	if _, err := zuliproundrobin.NewClients(); err == nil {
		t.Fatal("NewClients() error = nil, want error")
	}
	if _, err := zuliproundrobin.NewClients(nil); err == nil {
		t.Fatal("NewClients(nil) error = nil, want error")
	}
}

func TestClientUsesBaseAndWorkers(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	base.SetOwnUser(zulip.User{UserID: 10, Email: "base@example.com", FullName: "Base Bot", IsBot: true})
	firstWorker := zulipmock.NewClient()
	firstWorker.SetOwnUser(zulip.User{UserID: 20, Email: "first@example.com", FullName: "First Bot", IsBot: true})
	if _, _, err := firstWorker.CreateChannel(ctx).Name("first-worker").Execute(); err != nil {
		t.Fatalf("CreateChannel(first-worker) error = %v", err)
	}
	secondWorker := zulipmock.NewClient()
	secondWorker.SetOwnUser(zulip.User{UserID: 30, Email: "second@example.com", FullName: "Second Bot", IsBot: true})
	if _, _, err := secondWorker.CreateChannel(ctx).Name("second-worker").Execute(); err != nil {
		t.Fatalf("CreateChannel(second-worker) error = %v", err)
	}

	client, err := zuliproundrobin.NewClients(base, firstWorker, secondWorker)
	if err != nil {
		t.Fatalf("NewClients() error = %v", err)
	}

	var baseUserIDs []int64
	for range 5 {
		resp, _, err := client.GetOwnUser(ctx).Execute()
		if err != nil {
			t.Fatalf("GetOwnUser().Execute() error = %v", err)
		}
		baseUserIDs = append(baseUserIDs, resp.UserID)
	}
	wantBaseUserIDs := []int64{10, 10, 10, 10, 10}
	if !reflect.DeepEqual(baseUserIDs, wantBaseUserIDs) {
		t.Fatalf("base user IDs = %v, want %v", baseUserIDs, wantBaseUserIDs)
	}

	var workerChannels []string
	for range 5 {
		resp, _, err := client.GetChannels(ctx).Execute()
		if err != nil {
			t.Fatalf("GetChannels().Execute() error = %v", err)
		}
		if len(resp.Channels) != 1 {
			t.Fatalf("GetChannels() returned %d channels, want 1", len(resp.Channels))
		}
		workerChannels = append(workerChannels, resp.Channels[0].Name)
	}
	wantWorkerChannels := []string{"first-worker", "second-worker", "first-worker", "second-worker", "first-worker"}
	if !reflect.DeepEqual(workerChannels, wantWorkerChannels) {
		t.Fatalf("round-robin channel names = %v, want %v", workerChannels, wantWorkerChannels)
	}
}

func TestNewFromFilesLoadsBaseAndWorkers(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, key, ok := r.BasicAuth()
		if !ok {
			t.Error("request missing basic auth")
		}
		mu.Lock()
		requests = append(requests, email+":"+key)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{
			"result": "success",
			"msg":    "",
		}
		switch r.URL.Path {
		case "/api/v1/users/me":
			response["user_id"] = int64(len(email))
			response["email"] = email
			response["full_name"] = email
			response["is_bot"] = true
			response["role"] = int(zulip.RoleMember)
		case "/api/v1/streams":
			response["streams"] = []map[string]any{{"stream_id": int64(len(email)), "name": email}}
		default:
			t.Errorf("unexpected path = %q", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	firstPath := writeZulipRC(t, dir, "first.zuliprc", server.URL, "first@example.com", "first-key")
	secondPath := writeZulipRC(t, dir, "second.zuliprc", server.URL, "second@example.com", "second-key")

	client, err := zuliproundrobin.NewFromFiles(firstPath, secondPath)
	if err != nil {
		t.Fatalf("NewFromFiles() error = %v", err)
	}
	mu.Lock()
	requests = nil
	mu.Unlock()

	for range 3 {
		if _, _, err := client.GetOwnUser(context.Background()).Execute(); err != nil {
			t.Fatalf("GetOwnUser().Execute() error = %v", err)
		}
	}
	for range 3 {
		if _, _, err := client.GetChannels(context.Background()).Execute(); err != nil {
			t.Fatalf("GetChannels().Execute() error = %v", err)
		}
	}

	mu.Lock()
	got := append([]string(nil), requests...)
	mu.Unlock()
	want := []string{
		"first@example.com:first-key",
		"first@example.com:first-key",
		"first@example.com:first-key",
		"second@example.com:second-key",
		"second@example.com:second-key",
		"second@example.com:second-key",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated requests = %v, want %v", got, want)
	}
}

func TestNewWithWorkerFilesLoggerDoesNotUseDefaultLoggerForHTTPDumps(t *testing.T) {
	var defaultLogs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&defaultLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{
			"result": "success",
			"msg":    "",
		}
		switch r.URL.Path {
		case "/api/v1/users/me":
			response["user_id"] = int64(20)
			response["email"] = "worker@example.com"
			response["full_name"] = "Worker Bot"
			response["is_bot"] = true
			response["role"] = int(zulip.RoleMember)
		case "/api/v1/streams":
			response["streams"] = []map[string]any{{"stream_id": int64(1), "name": "worker-stream"}}
		default:
			t.Errorf("unexpected path = %q", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	base := zulipmock.NewClient()
	base.SetOwnUser(
		zulip.User{UserID: 10, Email: "base@example.com", FullName: "Base Bot", IsBot: true, Role: zulip.RoleMember},
	)
	workerPath := writeZulipRC(t, t.TempDir(), "worker.zuliprc", server.URL, "worker@example.com", "worker-key")
	workerLogger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := zuliproundrobin.NewWithWorkerFilesLogger(base, workerLogger, workerPath)
	if err != nil {
		t.Fatalf("NewWithWorkerFilesLogger() error = %v", err)
	}
	defaultLogs.Reset()

	if _, _, err := client.GetChannels(context.Background()).Execute(); err != nil {
		t.Fatalf("GetChannels().Execute() error = %v", err)
	}

	got := defaultLogs.String()
	if strings.Contains(got, "HTTP Request") || strings.Contains(got, "HTTP Response") {
		t.Fatalf("default logs contain HTTP dump: %q", got)
	}
}

func TestNewWithWorkerFilesLoggerAddsClientMetadata(t *testing.T) {
	var logs bytes.Buffer
	workerLogger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"result":  "success",
			"msg":     "",
			"streams": []map[string]any{{"stream_id": int64(1), "name": "worker-stream"}},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	base := zulipmock.NewClient()
	base.SetOwnUser(
		zulip.User{UserID: 10, Email: "base@example.com", FullName: "Base Bot", IsBot: true, Role: zulip.RoleMember},
	)
	workerPath := writeZulipRC(t, t.TempDir(), "worker.zuliprc", server.URL, "worker@example.com", "worker-key")

	client, err := zuliproundrobin.NewWithWorkerFilesLogger(base, workerLogger, workerPath)
	if err != nil {
		t.Fatalf("NewWithWorkerFilesLogger() error = %v", err)
	}

	if _, _, err := client.GetChannels(context.Background()).Execute(); err != nil {
		t.Fatalf("GetChannels().Execute() error = %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		"zulip_client_role=worker",
		"zulip_client_id=worker-1",
		"zulip_client_index=1",
		workerPath,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("worker log = %q, want to contain %q", got, want)
		}
	}
}

func TestNewFromFilesWarnsOnPermissionMismatch(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/users/me" {
			t.Errorf("path = %q, want /api/v1/users/me", r.URL.Path)
		}
		email, _, ok := r.BasicAuth()
		if !ok {
			t.Error("request missing basic auth")
		}
		role := zulip.RoleMember
		if email == "admin@example.com" {
			role = zulip.RoleAdmin
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"result":    "success",
			"msg":       "",
			"user_id":   int64(len(email)),
			"email":     email,
			"full_name": email,
			"is_bot":    true,
			"role":      int(role),
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	memberPath := writeZulipRC(t, dir, "member.zuliprc", server.URL, "member@example.com", "member-key")
	adminPath := writeZulipRC(t, dir, "admin.zuliprc", server.URL, "admin@example.com", "admin-key")

	if _, err := zuliproundrobin.NewFromFiles(memberPath, adminPath); err != nil {
		t.Fatalf("NewFromFiles() error = %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		"Zulip round-robin clients have mismatched permission levels",
		"reference_permission_level=member",
		"mismatched_permission_level=admin",
		memberPath,
		adminPath,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning log = %q, want to contain %q", got, want)
		}
	}
}

func writeZulipRC(t *testing.T, dir, name, site, email, key string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "[api]\n" +
		"site=" + site + "\n" +
		"email=" + email + "\n" +
		"key=" + key + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
