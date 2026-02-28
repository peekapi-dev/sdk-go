package peekapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tmpStoragePath returns a unique temp file path for each test to avoid flaky
// state leaking between tests.
func tmpStoragePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "peekapi-test.jsonl")
}

func makeEvent() RequestEvent {
	return RequestEvent{
		Method:         "GET",
		Path:           "/api/test",
		StatusCode:     200,
		ResponseTimeMs: 42.5,
		RequestSize:    100,
		ResponseSize:   256,
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// newTestClient creates a client pointing at the given test server URL with
// sensible test defaults (no periodic flush, large batch size).
func newTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	c, err := New(Options{
		APIKey:        "ak_test_key",
		Endpoint:      serverURL,
		FlushInterval: 1 * time.Hour, // don't auto-flush in tests
		BatchSize:     1000,          // don't auto-flush on size
		StoragePath:   tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c
}

// ─── Constructor Validation ──────────────────────────────────────────────────

func TestNew_EmptyEndpointUsesDefault(t *testing.T) {
	c, err := New(Options{APIKey: "test"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	c.Shutdown(context.Background())
}

func TestNew_RequiresValidURL(t *testing.T) {
	_, err := New(Options{APIKey: "test", Endpoint: "://bad"})
	if err == nil || !strings.Contains(err.Error(), "Invalid endpoint") {
		t.Fatalf("expected invalid URL error, got: %v", err)
	}
}

func TestNew_RejectsPlainHTTP(t *testing.T) {
	_, err := New(Options{APIKey: "test", Endpoint: "http://example.com/ingest"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected HTTPS enforcement error, got: %v", err)
	}
}

func TestNew_AllowsHTTPLocalhost(t *testing.T) {
	c, err := New(Options{
		APIKey:      "test",
		Endpoint:    "http://localhost:9999/ingest",
		StoragePath: tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("expected localhost HTTP to be allowed, got: %v", err)
	}
	c.Shutdown(context.Background())
}

func TestNew_AllowsHTTP127(t *testing.T) {
	c, err := New(Options{
		APIKey:      "test",
		Endpoint:    "http://127.0.0.1:9999/ingest",
		StoragePath: tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("expected 127.0.0.1 HTTP to be allowed, got: %v", err)
	}
	c.Shutdown(context.Background())
}

func TestNew_RejectsPrivateIPs(t *testing.T) {
	privateIPs := []string{
		"https://10.0.0.1/ingest",
		"https://172.16.0.1/ingest",
		"https://192.168.1.1/ingest",
		"https://169.254.1.1/ingest",
	}
	for _, endpoint := range privateIPs {
		_, err := New(Options{APIKey: "test", Endpoint: endpoint})
		if err == nil || !strings.Contains(err.Error(), "private") {
			t.Errorf("expected SSRF error for %s, got: %v", endpoint, err)
		}
	}
}

func TestNew_StripsEmbeddedCredentials(t *testing.T) {
	c, err := New(Options{
		APIKey:      "test",
		Endpoint:    "http://user:pass@localhost:9999/ingest",
		StoragePath: tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer c.Shutdown(context.Background())
	if c.parsedURL.User != nil {
		t.Fatal("expected credentials to be stripped")
	}
}

func TestNew_RequiresAPIKey(t *testing.T) {
	_, err := New(Options{Endpoint: "https://example.com/ingest"})
	if err == nil || !strings.Contains(err.Error(), "APIKey") {
		t.Fatalf("expected APIKey error, got: %v", err)
	}
}

func TestNew_RejectsInvalidAPIKeyChars(t *testing.T) {
	for _, key := range []string{"key\r", "key\n", "key\x00"} {
		_, err := New(Options{APIKey: key, Endpoint: "https://example.com/ingest"})
		if err == nil || !strings.Contains(err.Error(), "invalid characters") {
			t.Errorf("expected invalid char error for %q, got: %v", key, err)
		}
	}
}

// ─── Buffer Management ───────────────────────────────────────────────────────

func TestTrack_BuffersEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Track(makeEvent())

	if c.BufferLen() != 2 {
		t.Fatalf("expected 2 buffered events, got %d", c.BufferLen())
	}
}

func TestTrack_TriggersFlushWhenBufferIsFull(t *testing.T) {
	var flushCount atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushCount.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour, // disable timer flush
		BatchSize:     3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	// Track 3 events — should trigger a flush at batchSize
	for i := 0; i < 3; i++ {
		c.Track(makeEvent())
	}

	// Give the async flush goroutine time to fire
	time.Sleep(200 * time.Millisecond)

	if got := flushCount.Load(); got < 1 {
		t.Fatalf("expected at least 1 flush when buffer reached batchSize, got %d", got)
	}

	// Track 3 more — should trigger another flush
	for i := 0; i < 3; i++ {
		c.Track(makeEvent())
	}
	time.Sleep(200 * time.Millisecond)

	if got := flushCount.Load(); got < 2 {
		t.Fatalf("expected at least 2 flushes after 6 events with batchSize=3, got %d", got)
	}
}

func TestTrack_TruncatesInputs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)

	longPath := strings.Repeat("a", 3000)
	longMethod := strings.Repeat("X", 30)
	longConsumer := strings.Repeat("c", 300)

	c.Track(RequestEvent{
		Method:     longMethod,
		Path:       longPath,
		StatusCode: 200,
		ConsumerID: longConsumer,
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
	})

	c.bufMu.Lock()
	e := c.buffer[0]
	c.bufMu.Unlock()

	if len(e.Path) != maxPathLength {
		t.Errorf("expected path truncated to %d, got %d", maxPathLength, len(e.Path))
	}
	if len(e.Method) != maxMethodLength {
		t.Errorf("expected method truncated to %d, got %d", maxMethodLength, len(e.Method))
	}
	if len(e.ConsumerID) != maxConsumerIDLength {
		t.Errorf("expected consumer_id truncated to %d, got %d", maxConsumerIDLength, len(e.ConsumerID))
	}
}

// ─── Flush Behavior ──────────────────────────────────────────────────────────

func TestFlush_AutoFlushAtBatchSize(t *testing.T) {
	var receivedEvents []RequestEvent
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var events []RequestEvent
		json.Unmarshal(body, &events)
		mu.Lock()
		receivedEvents = append(receivedEvents, events...)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "ak_test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     3,
		StoragePath:   tmpStoragePath(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	for i := 0; i < 3; i++ {
		c.Track(makeEvent())
	}

	// Wait for async flush
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	count := len(receivedEvents)
	mu.Unlock()

	if count != 3 {
		t.Fatalf("expected 3 flushed events, got %d", count)
	}
}

func TestFlush_SendsCorrectPayload(t *testing.T) {
	var (
		gotContentType string
		gotAPIKey      string
		gotSDKHeader   string
		gotBody        []byte
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotAPIKey = r.Header.Get("x-api-key")
		gotSDKHeader = r.Header.Get("x-peekapi-sdk")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush()

	if gotContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", gotContentType)
	}
	if gotAPIKey != "ak_test_key" {
		t.Errorf("expected x-api-key ak_test_key, got %s", gotAPIKey)
	}
	if gotSDKHeader != "go/"+Version {
		t.Errorf("expected x-peekapi-sdk go/%s, got %s", Version, gotSDKHeader)
	}

	var events []RequestEvent
	if err := json.Unmarshal(gotBody, &events); err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event in payload, got %d", len(events))
	}
	if events[0].Path != "/api/test" {
		t.Errorf("expected path /api/test, got %s", events[0].Path)
	}
}

func TestFlush_SkipsEmpty(t *testing.T) {
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Flush()

	if requestCount != 0 {
		t.Fatalf("expected no requests for empty buffer, got %d", requestCount)
	}
}

func TestFlush_ConcurrentGuard(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // simulate slow server
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	for i := 0; i < 5; i++ {
		c.Track(makeEvent())
	}

	// Fire multiple concurrent flushes
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Flush()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if requestCount != 1 {
		t.Fatalf("expected exactly 1 flush request (concurrent guard), got %d", requestCount)
	}
}

// ─── Retry & Backoff ─────────────────────────────────────────────────────────

func TestFlush_ReinsertsOnFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Track(makeEvent())

	c.Flush()

	// Events should be re-inserted into buffer
	if c.BufferLen() != 2 {
		t.Fatalf("expected 2 events re-inserted, got %d", c.BufferLen())
	}
}

func TestFlush_BackoffAfterFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush() // first failure

	// Second flush should be skipped due to backoff
	c.Track(makeEvent())
	c.Flush()

	// Should still have events buffered (flush was skipped)
	if c.BufferLen() == 0 {
		t.Fatal("expected events to remain buffered during backoff")
	}
}

func TestFlush_ResetsOnSuccess(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 1 {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)

	// First: failure
	c.Track(makeEvent())
	c.Flush()

	c.mu.Lock()
	if c.consecutiveFailures != 1 {
		t.Fatalf("expected 1 consecutive failure, got %d", c.consecutiveFailures)
	}
	// Reset backoff so we can flush again immediately
	c.backoffUntil = time.Time{}
	c.mu.Unlock()

	// Second: success (events re-inserted from first failure)
	c.Flush()

	c.mu.Lock()
	failures := c.consecutiveFailures
	c.mu.Unlock()

	if failures != 0 {
		t.Fatalf("expected 0 consecutive failures after success, got %d", failures)
	}
}

// ─── Error Classification ────────────────────────────────────────────────────

func TestFlush_NonRetryable4xx_PersistsToDisk(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400) // non-retryable
	}))
	defer ts.Close()

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	c.Track(makeEvent())
	c.Flush()

	// Should NOT increment consecutiveFailures (non-retryable)
	c.mu.Lock()
	failures := c.consecutiveFailures
	c.mu.Unlock()
	bufLen := c.BufferLen()

	if failures != 0 {
		t.Fatalf("expected 0 consecutiveFailures for 400, got %d", failures)
	}
	if bufLen != 0 {
		t.Fatalf("expected empty buffer (events persisted to disk), got %d", bufLen)
	}

	// File should exist with persisted events
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		t.Fatal("expected storage file to exist after non-retryable error")
	}
}

func TestFlush_NonRetryable401_DoesNotRetry(t *testing.T) {
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(401) // non-retryable
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush()

	// Should not set backoff — next flush should proceed immediately
	c.Track(makeEvent())
	c.Flush()

	// Both flushes should have made requests (no backoff applied)
	if requestCount != 2 {
		t.Fatalf("expected 2 requests (no backoff on 401), got %d", requestCount)
	}
}

func TestFlush_Retryable429_ReinsertsBuffer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429) // retryable
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush()

	c.mu.Lock()
	failures := c.consecutiveFailures
	c.mu.Unlock()

	if failures != 1 {
		t.Fatalf("expected 1 consecutiveFailure for 429, got %d", failures)
	}
	if c.BufferLen() == 0 {
		t.Fatal("expected events re-inserted into buffer for retryable 429")
	}
}

func TestFlush_Retryable503_ReinsertsBuffer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // retryable
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush()

	c.mu.Lock()
	failures := c.consecutiveFailures
	c.mu.Unlock()

	if failures != 1 {
		t.Fatalf("expected 1 consecutiveFailure for 503, got %d", failures)
	}
	if c.BufferLen() == 0 {
		t.Fatal("expected events re-inserted into buffer for retryable 503")
	}
}

// ─── Backoff Jitter ─────────────────────────────────────────────────────────

func TestFlush_BackoffHasJitter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	// Collect backoff delays from multiple clients
	delays := make([]time.Duration, 10)
	for i := 0; i < 10; i++ {
		c := newTestClient(t, ts.URL)
		c.Track(makeEvent())

		before := time.Now()
		c.Flush()

		c.mu.Lock()
		delay := c.backoffUntil.Sub(before)
		c.mu.Unlock()
		delays[i] = delay
	}

	// With jitter, not all delays should be identical
	unique := make(map[time.Duration]bool)
	for _, d := range delays {
		unique[d.Round(time.Millisecond)] = true
	}
	if len(unique) <= 1 {
		t.Fatalf("expected jittered delays to vary, but all were identical: %v", delays)
	}

	// All delays should be in range [500ms, 1000ms] (base=1s, jitter 0.5-1.0)
	for _, d := range delays {
		if d < 450*time.Millisecond || d > 1100*time.Millisecond {
			t.Fatalf("delay %v out of expected jitter range [450ms, 1100ms]", d)
		}
	}
}

func TestFlush_NoBackoffOnNonRetryableError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403) // non-retryable
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Flush()

	c.mu.Lock()
	backoff := c.backoffUntil
	c.mu.Unlock()

	if !backoff.IsZero() {
		t.Fatalf("expected no backoff for non-retryable error, got %v", backoff)
	}
}

// ─── Disk Persistence ────────────────────────────────────────────────────────

func TestPersistToDisk_AfterMaxFailures(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	c.Track(makeEvent())

	// Simulate max consecutive failures
	for i := 0; i < maxConsecutiveFailures; i++ {
		c.mu.Lock()
		c.backoffUntil = time.Time{} // bypass backoff
		c.flushInFlight = false
		c.mu.Unlock()
		c.Flush()
	}

	// Check file was created
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		t.Fatal("expected storage file to exist after max failures")
	}

	data, err := os.ReadFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty storage file")
	}

	// Verify it's valid JSONL
	var events []RequestEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &events); err != nil {
		t.Fatalf("storage file is not valid JSON: %v", err)
	}
}

func TestLoadFromDisk_RecoversPersisted(t *testing.T) {
	sp := tmpStoragePath(t)

	// Write a JSONL file
	events := []RequestEvent{makeEvent(), makeEvent()}
	data, _ := json.Marshal(events)
	os.WriteFile(sp, append(data, '\n'), 0600)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	if c.BufferLen() != 2 {
		t.Fatalf("expected 2 recovered events, got %d", c.BufferLen())
	}

	// File should be deleted
	if _, err := os.Stat(sp); !os.IsNotExist(err) {
		t.Fatal("expected storage file to be deleted after loading")
	}
}

func TestLoadFromDisk_RespectsMaxBufferSize(t *testing.T) {
	sp := tmpStoragePath(t)

	events := make([]RequestEvent, 10)
	for i := range events {
		events[i] = makeEvent()
	}
	data, _ := json.Marshal(events)
	os.WriteFile(sp, append(data, '\n'), 0600)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		MaxBufferSize: 5,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	if c.BufferLen() != 5 {
		t.Fatalf("expected 5 events (maxBufferSize), got %d", c.BufferLen())
	}
}

func TestLoadFromDisk_HandlesCorruptFile(t *testing.T) {
	sp := tmpStoragePath(t)
	os.WriteFile(sp, []byte("not valid json\n"), 0600)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	if c.BufferLen() != 0 {
		t.Fatalf("expected 0 events from corrupt file, got %d", c.BufferLen())
	}
}

func TestPersistToDisk_RespectsMaxStorageBytes(t *testing.T) {
	sp := tmpStoragePath(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:          "test",
		Endpoint:        ts.URL,
		FlushInterval:   1 * time.Hour,
		BatchSize:       1000,
		StoragePath:     sp,
		MaxStorageBytes: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	// Write a file that's already at the limit AFTER construction
	// (constructor's loadFromDisk would delete it)
	os.WriteFile(sp, make([]byte, 100), 0600)

	// persistToDisk should skip because file is already over limit
	c.mu.Lock()
	c.persistToDisk([]RequestEvent{makeEvent()})
	c.mu.Unlock()

	info, _ := os.Stat(sp)
	if info.Size() != 100 {
		t.Fatalf("expected file size unchanged at 100, got %d", info.Size())
	}
}

func TestShutdown_PersistsBufferOnFlushFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Track(makeEvent())
	c.Track(makeEvent())
	err = c.Shutdown(context.Background())

	// Shutdown should report that events went to disk, not the network
	if !errors.Is(err, ErrEventsPersisted) {
		t.Fatalf("expected ErrEventsPersisted, got: %v", err)
	}

	// Events should be persisted to disk
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		t.Fatal("expected storage file to exist after shutdown with pending events")
	}
}

func TestLoadFromDisk_MultiLineJSONL(t *testing.T) {
	sp := tmpStoragePath(t)

	batch1, _ := json.Marshal([]RequestEvent{makeEvent()})
	batch2, _ := json.Marshal([]RequestEvent{makeEvent(), makeEvent()})
	content := string(batch1) + "\n" + string(batch2) + "\n"
	os.WriteFile(sp, []byte(content), 0600)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	if c.BufferLen() != 3 {
		t.Fatalf("expected 3 events from multi-line JSONL, got %d", c.BufferLen())
	}
}

func TestLoadFromDisk_RuntimeRecovery(t *testing.T) {
	sp := tmpStoragePath(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	// Simulate events persisted to disk mid-process
	events := []RequestEvent{makeEvent()}
	data, _ := json.Marshal(events)
	os.WriteFile(sp, append(data, '\n'), 0600)

	// Trigger runtime recovery on the same client
	c.loadFromDisk()

	if c.BufferLen() != 1 {
		t.Fatalf("expected 1 recovered event, got %d", c.BufferLen())
	}
}

// ─── Shutdown ────────────────────────────────────────────────────────────────

func TestShutdown_StopsBackgroundLoop(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	err := c.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("shutdown error: %v", err)
	}

	// Verify goroutine stopped by checking the stopped channel
	select {
	case <-c.stopped:
		// OK
	default:
		t.Fatal("expected background loop to have stopped")
	}
}

func TestShutdown_FlushesBuffer(t *testing.T) {
	var receivedEvents []RequestEvent
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var events []RequestEvent
		json.Unmarshal(body, &events)
		mu.Lock()
		receivedEvents = append(receivedEvents, events...)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Track(makeEvent())

	c.Shutdown(context.Background())

	mu.Lock()
	count := len(receivedEvents)
	mu.Unlock()

	if count != 2 {
		t.Fatalf("expected 2 events flushed on shutdown, got %d", count)
	}
}

// ─── SSRF Protection ─────────────────────────────────────────────────────────

func TestIsPrivateIP_StandardRanges(t *testing.T) {
	privateIPs := []string{
		"127.0.0.1", "127.255.255.255",
		"10.0.0.1", "10.255.255.255",
		"172.16.0.1", "172.31.255.255",
		"192.168.0.1", "192.168.255.255",
		"169.254.1.1",
		"0.0.0.0",
	}
	for _, ip := range privateIPs {
		if !IsPrivateIP(ip) {
			t.Errorf("expected %s to be private", ip)
		}
	}
}

func TestIsPrivateIP_CGNAT(t *testing.T) {
	cgnatIPs := []string{
		"100.64.0.1",
		"100.100.100.100",
		"100.127.255.255",
	}
	for _, ip := range cgnatIPs {
		if !IsPrivateIP(ip) {
			t.Errorf("expected CGNAT %s to be private", ip)
		}
	}

	// 100.128.0.0 is outside CGNAT (100.64.0.0/10)
	if IsPrivateIP("100.128.0.1") {
		t.Error("expected 100.128.0.1 to be public (outside CGNAT)")
	}
}

func TestIsPrivateIP_IPv6(t *testing.T) {
	privateIPv6 := []string{
		"::1",               // loopback
		"fc00::1",           // ULA
		"fd12:3456:789a::1", // ULA
		"fe80::1",           // link-local
	}
	for _, ip := range privateIPv6 {
		if !IsPrivateIP(ip) {
			t.Errorf("expected IPv6 %s to be private", ip)
		}
	}
}

func TestIsPrivateIP_IPv4MappedIPv6(t *testing.T) {
	mappedIPs := []string{
		"::ffff:10.0.0.1",
		"::ffff:127.0.0.1",
		"::ffff:192.168.1.1",
		"::ffff:172.16.0.1",
		"::ffff:100.64.0.1",
	}
	for _, ip := range mappedIPs {
		if !IsPrivateIP(ip) {
			t.Errorf("expected IPv4-mapped IPv6 %s to be private", ip)
		}
	}
}

func TestIsPrivateIP_PublicIPs(t *testing.T) {
	publicIPs := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
		"203.0.113.1",
	}
	for _, ip := range publicIPs {
		if IsPrivateIP(ip) {
			t.Errorf("expected %s to be public", ip)
		}
	}
}

func TestNew_BlocksCGNAT(t *testing.T) {
	_, err := New(Options{APIKey: "test", Endpoint: "https://100.64.0.1/ingest"})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected SSRF error for CGNAT IP, got: %v", err)
	}
}

func TestNew_BlocksIPv6Loopback(t *testing.T) {
	_, err := New(Options{APIKey: "test", Endpoint: "https://[::1]/ingest"})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected SSRF error for IPv6 loopback, got: %v", err)
	}
}

func TestNew_BlocksIPv6ULA(t *testing.T) {
	_, err := New(Options{APIKey: "test", Endpoint: "https://[fc00::1]/ingest"})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected SSRF error for IPv6 ULA, got: %v", err)
	}
}

func TestIsPrivateIPAddr_Numeric(t *testing.T) {
	// Test the internal numeric checker directly
	tests := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.128.0.1", false},
		{"172.15.255.255", false},
		{"172.16.0.1", true},
		{"172.32.0.1", false},
		{"192.168.0.1", true},
		{"8.8.8.8", false},
		{"::1", true},
		{"fc00::1", true},
		{"fe80::1", true},
		{"2001:4860:4860::8888", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("failed to parse IP %s", tt.ip)
		}
		got := isPrivateIPAddr(ip)
		if got != tt.private {
			t.Errorf("isPrivateIPAddr(%s) = %v, want %v", tt.ip, got, tt.private)
		}
	}
}

// ─── FlushContext ────────────────────────────────────────────────────────────

func TestFlushContext_RespectsContextCancellation(t *testing.T) {
	// Use an already-cancelled context — no server connection needed
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())

	// Cancel the context before calling FlushContext
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := c.FlushContext(ctx)

	// Flush should have failed due to context cancellation
	if err == nil {
		t.Fatal("expected FlushContext to fail with cancelled context")
	}

	// Events should be re-inserted (network error is retryable)
	if c.BufferLen() == 0 {
		t.Fatal("expected events to be re-inserted after context cancellation")
	}
}

func TestFlushContext_BackwardsCompat(t *testing.T) {
	// Verify Flush() still works as before (delegates to FlushContext(background))
	var received int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var events []RequestEvent
		json.Unmarshal(body, &events)
		received = len(events)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Track(makeEvent())

	err := c.Flush()
	if err != nil {
		t.Fatalf("Flush() failed: %v", err)
	}
	if received != 2 {
		t.Fatalf("expected 2 events via Flush(), got %d", received)
	}
}

func TestShutdownSync_AttemptsNetworkFlush(t *testing.T) {
	var received int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var events []RequestEvent
		json.Unmarshal(body, &events)
		mu.Lock()
		received += len(events)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())
	c.Track(makeEvent())

	// Call shutdownSync (used by signal handlers) — should attempt network flush
	c.shutdownSync()

	mu.Lock()
	r := received
	mu.Unlock()

	if r != 2 {
		t.Fatalf("expected shutdownSync to deliver 2 events via network, got %d", r)
	}
}

func TestShutdownSync_FallsBackToDiskOnTimeout(t *testing.T) {
	// Server that blocks until done channel is closed (cleanup unblocks it)
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	t.Cleanup(func() {
		close(done)
		ts.Close()
	})

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Track(makeEvent())

	// shutdownSync should timeout (2s context) and persist to disk
	c.shutdownSync()

	if _, err := os.Stat(sp); os.IsNotExist(err) {
		t.Fatal("expected events to be persisted to disk after shutdownSync timeout")
	}
}

func TestShutdown_PersistsToDiskWhenFlushContextExpires(t *testing.T) {
	// Server that blocks until done channel is closed (cleanup unblocks it)
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))
	t.Cleanup(func() {
		close(done)
		ts.Close()
	})

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Track(makeEvent())

	// Shutdown with a very short timeout — flush will fail, events go to disk
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err = c.Shutdown(ctx)

	// Shutdown should report that events went to disk, not the network
	if !errors.Is(err, ErrEventsPersisted) {
		t.Fatalf("expected ErrEventsPersisted, got: %v", err)
	}

	// Events should be persisted to disk (flush failed due to context timeout)
	if _, err := os.Stat(sp); os.IsNotExist(err) {
		t.Fatal("expected events to be persisted to disk when shutdown flush context expires")
	}
}

// ─── OnError Callback ────────────────────────────────────────────────────────

func TestOnError_CalledOnFlushFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()

	var errCount atomic.Int32
	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 50 * time.Millisecond, // short interval to trigger timer flush
		BatchSize:     1000,
		StoragePath:   sp,
		OnError: func(err error) {
			errCount.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	c.Track(makeEvent())

	// Wait for at least one timer-triggered flush to fail
	time.Sleep(200 * time.Millisecond)

	if got := errCount.Load(); got < 1 {
		t.Fatalf("expected OnError to be called at least once, got %d calls", got)
	}
}

func TestOnError_NotCalledOnSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	var errCount atomic.Int32
	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
		OnError: func(err error) {
			errCount.Add(1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	c.Track(makeEvent())
	c.Flush()

	if got := errCount.Load(); got != 0 {
		t.Fatalf("expected OnError not to be called on success, got %d calls", got)
	}
}

// ─── Shutdown Error Reporting ─────────────────────────────────────────────────

func TestShutdown_ReturnsNilOnSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)
	c.Track(makeEvent())

	err := c.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("expected nil on successful shutdown, got: %v", err)
	}
}

func TestShutdown_ReturnsErrEventsPersisted_NonRetryable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400) // non-retryable — FlushContext persists internally
	}))
	defer ts.Close()

	sp := tmpStoragePath(t)
	c, err := New(Options{
		APIKey:        "test",
		Endpoint:      ts.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   sp,
	})
	if err != nil {
		t.Fatal(err)
	}

	c.Track(makeEvent())

	err = c.Shutdown(context.Background())
	if !errors.Is(err, ErrEventsPersisted) {
		t.Fatalf("expected ErrEventsPersisted for non-retryable error, got: %v", err)
	}
}

// ─── sync.Pool / Multiple Flushes ────────────────────────────────────────────

func TestFlush_MultipleFlushesReuseBuffers(t *testing.T) {
	var mu sync.Mutex
	received := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var events []RequestEvent
		if err := json.NewDecoder(r.Body).Decode(&events); err == nil {
			mu.Lock()
			received += len(events)
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	c := newTestClient(t, ts.URL)

	// Flush multiple times — exercises sync.Pool buffer reuse
	for i := 0; i < 10; i++ {
		c.Track(makeEvent())
		if err := c.Flush(); err != nil {
			t.Fatalf("flush %d failed: %v", i, err)
		}
	}

	mu.Lock()
	r := received
	mu.Unlock()
	if r != 10 {
		t.Errorf("expected 10 events delivered across flushes, got %d", r)
	}
}

// ─── HashConsumerID ──────────────────────────────────────────────────────────

func TestHashConsumerID_Format(t *testing.T) {
	hash := HashConsumerID("Bearer eyJtoken123")
	if !strings.HasPrefix(hash, "hash_") {
		t.Errorf("expected hash_ prefix, got %s", hash)
	}
	// "hash_" (5) + 12 hex chars = 17
	if len(hash) != 17 {
		t.Errorf("expected length 17, got %d (%s)", len(hash), hash)
	}
}

func TestHashConsumerID_Deterministic(t *testing.T) {
	h1 := HashConsumerID("Bearer abc123")
	h2 := HashConsumerID("Bearer abc123")
	if h1 != h2 {
		t.Errorf("expected same hash for same input, got %s vs %s", h1, h2)
	}
}

func TestHashConsumerID_DifferentForDifferentInputs(t *testing.T) {
	h1 := HashConsumerID("Bearer token1")
	h2 := HashConsumerID("Bearer token2")
	if h1 == h2 {
		t.Errorf("expected different hashes for different inputs")
	}
}

// ─── DefaultIdentifyConsumer ─────────────────────────────────────────────────

func TestDefaultIdentifyConsumer_PrefersAPIKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-API-Key", "my-key")
	r.Header.Set("Authorization", "Bearer token")

	id := DefaultIdentifyConsumer(r)
	if id != "my-key" {
		t.Errorf("expected 'my-key', got '%s'", id)
	}
}

func TestDefaultIdentifyConsumer_HashesAuthorization(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer secret")

	id := DefaultIdentifyConsumer(r)
	if !strings.HasPrefix(id, "hash_") {
		t.Errorf("expected hashed Authorization, got '%s'", id)
	}
}

func TestDefaultIdentifyConsumer_EmptyWhenNoHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	id := DefaultIdentifyConsumer(r)
	if id != "" {
		t.Errorf("expected empty consumer ID, got '%s'", id)
	}
}
