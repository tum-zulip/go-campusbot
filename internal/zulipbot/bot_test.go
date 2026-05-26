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
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
)

const (
	ownUserPath        = "/api/v1/users/me"
	sendMessagePath    = "/api/v1/messages"
	testChannelContent = "Hello Zulip community"
	testChannelID      = int64(456)
	testChannelTopic   = "introductions"
	testMessageID      = int64(789)
	testUserEmail      = "bot@example.com"
	testUserID         = int64(123)
	testTimeout        = time.Second
)

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

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	bot, err := zulipbot.New(ctx, zulipbot.RuntimeConfig{
		RCPath: rcPath,
		Logger: newTestLogger(),
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
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
