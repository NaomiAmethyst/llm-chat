// SPDX-License-Identifier: GPL-3.0-only
// Copyright (C) 2026 Naomi Persephone Amethyst <naomi@amethyst.name>

package main

// Tests for -key-cmd: obtaining the key at startup and refreshing it when a
// request fails.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rotatingKeyCmd returns a shell command that prints "key1", then "key2", ...
// on successive runs, simulating a credential helper whose key rotates.
func rotatingKeyCmd(t *testing.T) string {
	t.Helper()
	counter := t.TempDir() + "/n"
	return fmt.Sprintf(`n=$(cat %[1]q 2>/dev/null || echo 0); n=$((n+1)); printf %%s "$n" > %[1]q; echo "key$n"`, counter)
}

// keyGatedServer accepts only the given bearer token and counts requests.
func keyGatedServer(t *testing.T, want string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"authorized"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunKeyCmd(t *testing.T) {
	s := &Session{keyCmd: "echo secret-key"}
	key, err := s.runKeyCmd()
	if err != nil {
		t.Fatal(err)
	}
	if key != "secret-key" {
		t.Errorf("key = %q", key)
	}

	// Helpers often print the secret first and metadata after.
	s.keyCmd = "printf 'first-line\\nuser: someone\\n'"
	if key, err = s.runKeyCmd(); err != nil || key != "first-line" {
		t.Errorf("key = %q, err = %v", key, err)
	}

	s.keyCmd = "true" // no output
	if _, err = s.runKeyCmd(); err == nil {
		t.Error("expected an error when the command prints nothing")
	}

	s.keyCmd = "echo 'no such vault' >&2; exit 3"
	_, err = s.runKeyCmd()
	if err == nil || !strings.Contains(err.Error(), "no such vault") {
		t.Errorf("stderr not reported: %v", err)
	}
}

// The command is told which endpoint the key is for.
func TestRunKeyCmdEndpointEnv(t *testing.T) {
	s := &Session{keyCmd: `echo "key-for-$LLM_CHAT_ENDPOINT"`, baseURL: "https://example/v1"}
	key, err := s.runKeyCmd()
	if err != nil {
		t.Fatal(err)
	}
	if key != "key-for-https://example/v1" {
		t.Errorf("key = %q", key)
	}
}

func TestRefreshKeyReportsChange(t *testing.T) {
	s := &Session{keyCmd: rotatingKeyCmd(t)}
	if s.refreshKey() != true || s.apiKey != "key1" {
		t.Fatalf("first refresh: changed=%v key=%q", true, s.apiKey)
	}
	if !s.refreshKey() || s.apiKey != "key2" {
		t.Errorf("rotation not adopted: %q", s.apiKey)
	}
	// A command whose output is stable must not report a change.
	stable := &Session{keyCmd: "echo same", apiKey: "same"}
	if stable.refreshKey() {
		t.Error("unchanged key reported as changed")
	}
	// No -key-cmd configured: nothing to refresh.
	if (&Session{}).refreshKey() {
		t.Error("refreshKey should be a no-op without -key-cmd")
	}
}

// A rotated key must be picked up and the failed request retried with it.
func TestSendRetriesWithRefreshedKey(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key2", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", keyCmd: rotatingKeyCmd(t)}
	s.refreshKey() // startup: key1, which the server rejects
	if s.apiKey != "key1" {
		t.Fatalf("startup key = %q", s.apiKey)
	}
	s.push("user", "hi")

	var content string
	captureStdout(t, func() {
		var err error
		content, _, err = s.send(context.Background())
		if err != nil {
			t.Errorf("send after key refresh: %v", err)
		}
	})
	if content != "authorized" {
		t.Errorf("content = %q", content)
	}
	if s.apiKey != "key2" {
		t.Errorf("session kept the stale key: %q", s.apiKey)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("request count = %d, want 2 (original + retry)", got)
	}
}

// If the command returns the same key, retrying would just fail again.
func TestSendDoesNotRetryWhenKeyUnchanged(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "other", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", keyCmd: "echo stable", apiKey: "stable"}
	s.push("user", "hi")
	if _, _, err := s.send(context.Background()); err == nil {
		t.Fatal("expected the request to fail")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("request count = %d, want 1 (no pointless retry)", got)
	}
}

func TestSendDoesNotRetryWithoutKeyCmd(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "other", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", apiKey: "wrong"}
	s.push("user", "hi")
	if _, _, err := s.send(context.Background()); err == nil {
		t.Fatal("expected the request to fail")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("request count = %d, want 1", got)
	}
}

// Retrying after part of an answer was printed would duplicate it.
func TestSendDoesNotRetryAfterPartialContent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"half an answer\"}}]}\n\n")
		fmt.Fprint(w, "data: {broken\n\n")
	}))
	defer srv.Close()
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", stream: true, keyCmd: rotatingKeyCmd(t)}
	s.push("user", "hi")

	var content string
	captureStdout(t, func() {
		var err error
		content, _, err = s.send(context.Background())
		if err == nil {
			t.Error("expected the malformed stream to error")
		}
	})
	if content != "half an answer" {
		t.Errorf("content = %q", content)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("request count = %d, want 1 (a printed answer must not be re-sent)", got)
	}
}

// A cancelled request (Ctrl+C) must not be retried either.
func TestSendDoesNotRetryWhenCancelled(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key2", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", keyCmd: rotatingKeyCmd(t)}
	s.push("user", "hi")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.send(ctx); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("request count = %d, want 0", got)
	}
}

func TestFetchModelsRetriesWithRefreshedKey(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key2", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, keyCmd: rotatingKeyCmd(t)}
	s.refreshKey() // key1: rejected
	ids, err := s.fetchModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("ids = %v", ids)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("request count = %d, want 2", got)
	}
}

// -key-cmd must survive an endpoint switch: env lookup never overrides it.
func TestKeyCmdSurvivesEndpointChange(t *testing.T) {
	clearKeyEnv(t)
	t.Setenv("OPENROUTER_API_KEY", "env-key")
	s := &Session{baseURL: "http://127.0.0.1:18789/v1", keyCmd: "echo cmd-key", keyLocked: true}
	s.refreshKey()
	if _, err := s.loadFromText(
		`{"endpoint":"https://openrouter.ai/api/v1","messages":[{"role":"user","content":"hi"}]}`); err != nil {
		t.Fatal(err)
	}
	if s.apiKey != "cmd-key" {
		t.Errorf("env key overrode -key-cmd: %q", s.apiKey)
	}
}

// ---------------------------------------------------------------------------
// -key-ttl: proactive refresh

// With a TTL shorter than the gap between requests, the key is renewed before
// it is used, so a rotated credential never costs a failed request.
func TestKeyTTLRefreshesBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key2", &hits)
	s := &Session{
		client: srv.Client(), baseURL: srv.URL, model: "m",
		keyCmd: rotatingKeyCmd(t), keyTTL: time.Hour,
	}
	if err := s.initKey(); err != nil { // startup: key1, which the server rejects
		t.Fatal(err)
	}
	// Expire the key explicitly instead of relying on the platform clock resolution.
	s.keyFetched = time.Now().Add(-2 * s.keyTTL)
	s.push("user", "hi")

	var content string
	captureStdout(t, func() {
		var err error
		content, _, err = s.send(context.Background())
		if err != nil {
			t.Errorf("send: %v", err)
		}
	})
	if content != "authorized" {
		t.Errorf("content = %q", content)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("request count = %d, want 1 (refreshed before sending, no failure needed)", got)
	}
}

// A key still inside its TTL must not trigger the helper again.
func TestKeyTTLSkipsFreshKey(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key1", &hits)
	s := &Session{
		client: srv.Client(), baseURL: srv.URL, model: "m",
		keyCmd: rotatingKeyCmd(t), keyTTL: time.Hour,
	}
	if err := s.initKey(); err != nil {
		t.Fatal(err)
	}
	s.push("user", "hi")
	for range 3 {
		captureStdout(t, func() {
			if _, _, err := s.send(context.Background()); err != nil {
				t.Errorf("send: %v", err)
			}
		})
	}
	if s.apiKey != "key1" {
		t.Errorf("key rotated inside its TTL: %q", s.apiKey)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("request count = %d, want 3 (no retries)", got)
	}
}

// Without a TTL the helper only runs on failure, as before.
func TestNoTTLMeansNoProactiveRefresh(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key1", &hits)
	s := &Session{client: srv.Client(), baseURL: srv.URL, model: "m", keyCmd: rotatingKeyCmd(t)}
	if err := s.initKey(); err != nil {
		t.Fatal(err)
	}
	s.push("user", "hi")
	captureStdout(t, func() {
		if _, _, err := s.send(context.Background()); err != nil {
			t.Errorf("send: %v", err)
		}
	})
	if s.apiKey != "key1" {
		t.Errorf("key refreshed without a TTL or a failure: %q", s.apiKey)
	}
}

func TestRefreshKeyIfStaleIsNoOpWithoutKeyCmd(t *testing.T) {
	s := &Session{apiKey: "static", keyTTL: time.Nanosecond}
	s.refreshKeyIfStale()
	if s.apiKey != "static" {
		t.Errorf("key = %q", s.apiKey)
	}
}

// The TTL is proactive; the failure-triggered refresh remains the backstop.
func TestKeyTTLAndFailureRetryCoexist(t *testing.T) {
	var hits atomic.Int32
	srv := keyGatedServer(t, "key3", &hits)
	s := &Session{
		client: srv.Client(), baseURL: srv.URL, model: "m",
		keyCmd: rotatingKeyCmd(t), keyTTL: time.Hour,
	}
	if err := s.initKey(); err != nil { // key1
		t.Fatal(err)
	}
	// Expire the key explicitly instead of relying on the platform clock resolution.
	s.keyFetched = time.Now().Add(-2 * s.keyTTL)
	s.push("user", "hi")
	captureStdout(t, func() {
		// key2 from the proactive refresh is still wrong; the failure path
		// then supplies key3 and retries.
		if _, _, err := s.send(context.Background()); err != nil {
			t.Errorf("send: %v", err)
		}
	})
	if s.apiKey != "key3" {
		t.Errorf("key = %q, want key3", s.apiKey)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("request count = %d, want 2", got)
	}
}

func TestEnvDuration(t *testing.T) {
	t.Setenv("LLM_CHAT_KEY_TTL", "55m")
	if got := envDuration("LLM_CHAT_KEY_TTL"); got != 55*time.Minute {
		t.Errorf("got %v", got)
	}
	t.Setenv("LLM_CHAT_KEY_TTL", "")
	if got := envDuration("LLM_CHAT_KEY_TTL"); got != 0 {
		t.Errorf("unset should be 0, got %v", got)
	}
	t.Setenv("LLM_CHAT_KEY_TTL", "not-a-duration")
	if got := envDuration("LLM_CHAT_KEY_TTL"); got != 0 {
		t.Errorf("invalid should be 0, got %v", got)
	}
}
