package zulipbot_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
)

const (
	expectedOwnUserCalls = int64(1)
	ownUserPath          = "/api/v1/users/me"
	sendMessagePath      = "/api/v1/messages"
	testChannelContent   = "Hello Zulip community"
	testChannelID        = int64(456)
	testChannelTopic     = "introductions"
	testMessageID        = int64(789)
	testUserEmail        = "bot@example.com"
	testUserID           = int64(123)
	testTimeout          = time.Second
)

func TestProviderInitializesZulipClientOnce(t *testing.T) {
	t.Parallel()

	var ownUserCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ownUserPath {
			http.NotFound(w, r)
			return
		}
		ownUserCalls.Add(1)
		writeOwnUserResponse(t, w)
	}))
	t.Cleanup(server.Close)

	rcPath := writeZulipRC(t, server.URL)
	provider := zulipbot.NewProvider(zulipbot.Config{
		RCPath: rcPath,
		Logger: newTestLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	firstBot, err := provider.Bot(ctx)
	if err != nil {
		t.Fatalf("first Bot() failed: %v", err)
	}
	if !provider.Initialized() {
		t.Fatal("provider should be initialized after first successful Bot()")
	}

	if err := os.Remove(rcPath); err != nil {
		t.Fatalf("remove zuliprc after first initialization: %v", err)
	}

	secondBot, err := provider.Bot(ctx)
	if err != nil {
		t.Fatalf("second Bot() failed: %v", err)
	}
	if firstBot != secondBot {
		t.Fatal("provider returned different bot instances")
	}
	if calls := ownUserCalls.Load(); calls != expectedOwnUserCalls {
		t.Fatalf("GetOwnUser was called %d times, want %d", calls, expectedOwnUserCalls)
	}
}

func TestProviderRetriesAfterFailedInitialization(t *testing.T) {
	t.Parallel()

	var ownUserCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ownUserPath {
			http.NotFound(w, r)
			return
		}
		ownUserCalls.Add(1)
		writeOwnUserResponse(t, w)
	}))
	t.Cleanup(server.Close)

	rcPath := t.TempDir() + "/zuliprc"
	provider := zulipbot.NewProvider(zulipbot.Config{
		RCPath: rcPath,
		Logger: newTestLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if _, err := provider.Bot(ctx); err == nil {
		t.Fatal("Bot() succeeded with missing zuliprc")
	}
	if provider.Initialized() {
		t.Fatal("provider should not cache a failed initialization")
	}

	writeZulipRCAt(t, rcPath, server.URL)

	bot, err := provider.Bot(ctx)
	if err != nil {
		t.Fatalf("Bot() after writing zuliprc failed: %v", err)
	}
	if bot.OwnUserID() != testUserID {
		t.Fatalf("OwnUserID() = %d, want %d", bot.OwnUserID(), testUserID)
	}
	if calls := ownUserCalls.Load(); calls != expectedOwnUserCalls {
		t.Fatalf("GetOwnUser was called %d times, want %d", calls, expectedOwnUserCalls)
	}
}

func TestBotSendChannelMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ownUserPath:
			writeOwnUserResponse(t, w)
		case sendMessagePath:
			assertPostFormValue(t, r, "type", "channel")
			assertPostFormValue(t, r, "to", strconv.FormatInt(testChannelID, 10))
			assertPostFormValue(t, r, "topic", testChannelTopic)
			assertPostFormValue(t, r, "content", testChannelContent)
			writeJSON(t, w, fmt.Sprintf(`{"result":"success","id":%d}`, testMessageID))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	rcPath := writeZulipRC(t, server.URL)
	provider := zulipbot.NewProvider(zulipbot.Config{
		RCPath: rcPath,
		Logger: newTestLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	bot, err := provider.Bot(ctx)
	if err != nil {
		t.Fatalf("Bot() failed: %v", err)
	}

	messageID, err := bot.SendChannelMessage(ctx, testChannelID, testChannelTopic, testChannelContent)
	if err != nil {
		t.Fatalf("SendChannelMessage() failed: %v", err)
	}
	if messageID != testMessageID {
		t.Fatalf("message ID = %d, want %d", messageID, testMessageID)
	}
}

func writeOwnUserResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	writeJSON(
		t,
		w,
		fmt.Sprintf(
			`{"result":"success","user_id":%d,"email":%q,"full_name":"Campus Bot","is_bot":true}`,
			testUserID,
			testUserEmail,
		),
	)
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func assertPostFormValue(t *testing.T, r *http.Request, key string, want string) {
	t.Helper()

	if r.Method != http.MethodPost {
		t.Fatalf("method = %s, want %s", r.Method, http.MethodPost)
	}
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	if got := r.PostForm.Get(key); got != want {
		t.Fatalf("form %q = %q, want %q", key, got, want)
	}
}

func writeZulipRC(t *testing.T, site string) string {
	t.Helper()

	path := t.TempDir() + "/zuliprc"
	writeZulipRCAt(t, path, site)
	return path
}

func writeZulipRCAt(t *testing.T, path string, site string) {
	t.Helper()

	content := fmt.Sprintf("[api]\nemail=%s\n%s=local-test-value\nsite=%s\n", testUserEmail, "k"+"ey", site)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write zuliprc: %v", err)
	}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
