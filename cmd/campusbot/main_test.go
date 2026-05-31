package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		wantBotLevel    slog.Level
		wantClientLevel slog.Level
	}{
		{
			name:            "verbose enables bot and Zulip client debug",
			input:           "verbose",
			wantBotLevel:    slog.LevelDebug,
			wantClientLevel: slog.LevelDebug,
		},
		{
			name:            "debug enables only bot debug",
			input:           "debug",
			wantBotLevel:    slog.LevelDebug,
			wantClientLevel: slog.LevelInfo,
		},
		{
			name:            "warning alias",
			input:           "warning",
			wantBotLevel:    slog.LevelWarn,
			wantClientLevel: slog.LevelWarn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogLevel(tt.input)
			if err != nil {
				t.Fatalf("parseLogLevel(%q) returned error: %v", tt.input, err)
			}
			if got.BotLevel != tt.wantBotLevel || got.ZulipClientLevel != tt.wantClientLevel {
				t.Fatalf("parseLogLevel(%q) = %+v, want bot=%s client=%s",
					tt.input,
					got,
					tt.wantBotLevel,
					tt.wantClientLevel,
				)
			}
		})
	}
}

func TestParseLogLevelRejectsUnknownLevel(t *testing.T) {
	if _, err := parseLogLevel("trace"); err == nil {
		t.Fatal("parseLogLevel(\"trace\") returned nil error")
	}
}

func TestParseLogFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  logFormatConfig
	}{
		{
			name:  "text",
			input: "text",
			want:  logFormatText,
		},
		{
			name:  "json",
			input: "json",
			want:  logFormatJSON,
		},
		{
			name:  "empty defaults to text",
			input: "",
			want:  logFormatText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLogFormat(tt.input)
			if err != nil {
				t.Fatalf("parseLogFormat(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("parseLogFormat(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseLogFormatRejectsUnknownFormat(t *testing.T) {
	if _, err := parseLogFormat("console"); err == nil {
		t.Fatal("parseLogFormat(\"console\") returned nil error")
	}
}

func TestNewLoggerSupportsJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, slog.LevelInfo, logFormatJSON)

	logger.Info("hello", "answer", 42)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log entry is not JSON: %v\nentry: %s", err, buf.String())
	}
	if entry["msg"] != "hello" {
		t.Fatalf("msg = %v, want hello", entry["msg"])
	}
	if entry["answer"] != float64(42) {
		t.Fatalf("answer = %v, want 42", entry["answer"])
	}
}

func TestWorkerZulipRCPathsDefaultsToNoWorkers(t *testing.T) {
	got, err := workerZulipRCPaths("", testLogger(t, nil))
	if err != nil {
		t.Fatalf("workerZulipRCPaths() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("workerZulipRCPaths() = %q, want no worker paths", got)
	}
}

func TestWorkerZulipRCPathsLoadsOnlyDirectoryZulipRCs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "b.zuliprc"))
	writeTestFile(t, filepath.Join(dir, "a.zuliprc"))
	writeTestFile(t, filepath.Join(dir, "zuliprc"))
	writeTestFile(t, filepath.Join(dir, "README.md"))
	if err := os.Mkdir(filepath.Join(dir, "nested.zuliprc"), 0o750); err != nil {
		t.Fatalf("mkdir nested.zuliprc: %v", err)
	}

	got, err := workerZulipRCPaths(dir, testLogger(t, nil))
	if err != nil {
		t.Fatalf("workerZulipRCPaths() error = %v", err)
	}
	want := []string{
		filepath.Join(dir, "a.zuliprc"),
		filepath.Join(dir, "b.zuliprc"),
		filepath.Join(dir, "zuliprc"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("workerZulipRCPaths() = %q, want %q", got, want)
	}
}

func TestWorkerZulipRCPathsWarnsAndReturnsNoWorkersWhenDirectoryHasNoZulipRCs(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "README.md"))
	var logs bytes.Buffer

	got, err := workerZulipRCPaths(dir, testLogger(t, &logs))
	if err != nil {
		t.Fatalf("workerZulipRCPaths() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("workerZulipRCPaths() = %q, want no worker paths", got)
	}
	if log := logs.String(); !strings.Contains(log, "using main Zulip client for background requests") {
		t.Fatalf("warning log = %q, want no-workers warning", log)
	}
}

func TestWorkerZulipRCPathsWarnsAndReturnsNoWorkersWhenDirectoryIsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	var logs bytes.Buffer

	got, err := workerZulipRCPaths(dir, testLogger(t, &logs))
	if err != nil {
		t.Fatalf("workerZulipRCPaths() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("workerZulipRCPaths() = %q, want no worker paths", got)
	}
	if log := logs.String(); !strings.Contains(log, "worker rc directory not found") {
		t.Fatalf("warning log = %q, want missing-directory warning", log)
	}
}

func TestResettableBodyTransportRewindsRequestBody(t *testing.T) {
	recorder := &bodyRecorderTransport{}
	transport := resettableBodyTransport{base: recorder}
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://example.invalid",
		strings.NewReader("payload"),
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	for range 2 {
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip() error = %v", err)
		}
		_ = resp.Body.Close()
	}

	if want := []string{"payload", "payload"}; !equalStrings(recorder.bodies, want) {
		t.Fatalf("recorded bodies = %q, want %q", recorder.bodies, want)
	}
}

type bodyRecorderTransport struct {
	bodies []string
}

func (t *bodyRecorderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	t.bodies = append(t.bodies, string(body))
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func testLogger(t *testing.T, w io.Writer) *slog.Logger {
	t.Helper()
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
