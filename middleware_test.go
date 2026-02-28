package peekapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMiddlewareTestClient creates a client with a test server and returns
// both the client and the test server URL.
func newMiddlewareTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	c, err := New(Options{
		APIKey:        "ak_test_key",
		Endpoint:      serverURL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c
}

// ─── Request Data Capture ────────────────────────────────────────────────────

func TestMiddleware_CapturesRequestData(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte("hello world"))
	}))

	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Length", "15")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if c.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", c.BufferLen())
	}

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if e.Method != "POST" {
		t.Errorf("expected method POST, got %s", e.Method)
	}
	if e.Path != "/api/users" {
		t.Errorf("expected path /api/users, got %s", e.Path)
	}
	if e.StatusCode != 201 {
		t.Errorf("expected status 201, got %d", e.StatusCode)
	}
	if e.ResponseSize != 11 { // len("hello world")
		t.Errorf("expected response size 11, got %d", e.ResponseSize)
	}
	if e.ResponseTimeMs < 0 {
		t.Errorf("expected non-negative response time, got %f", e.ResponseTimeMs)
	}
	if e.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
}

func TestMiddleware_DefaultStatusCode200(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't call WriteHeader — default is 200
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if e.StatusCode != 200 {
		t.Errorf("expected default status 200, got %d", e.StatusCode)
	}
}

// ─── Consumer Identification ─────────────────────────────────────────────────

func TestMiddleware_ConsumerFromAPIKey(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "consumer-key-123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if e.ConsumerID != "consumer-key-123" {
		t.Errorf("expected consumer_id 'consumer-key-123', got '%s'", e.ConsumerID)
	}
}

func TestMiddleware_ConsumerHashesAuthorization(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer super-secret-jwt")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if !strings.HasPrefix(e.ConsumerID, "hash_") {
		t.Errorf("expected hashed consumer_id, got '%s'", e.ConsumerID)
	}
	// Should NOT contain the raw token
	if strings.Contains(e.ConsumerID, "super-secret-jwt") {
		t.Error("consumer_id should not contain raw Authorization value")
	}
}

func TestMiddleware_CustomIdentifyConsumer(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c, err := New(Options{
		APIKey:        "ak_test_key",
		Endpoint:      ingestServer.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   tmpStoragePath(t),
		IdentifyConsumer: func(r *http.Request) string {
			return r.Header.Get("X-Tenant-ID")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "tenant-42")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if e.ConsumerID != "tenant-42" {
		t.Errorf("expected consumer_id 'tenant-42', got '%s'", e.ConsumerID)
	}
}

// ─── ResponseWriter Wrapper ──────────────────────────────────────────────────

func TestResponseWriter_CapturesMultipleWrites(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("chunk1"))
		w.Write([]byte("chunk2"))
		w.Write([]byte("chunk3"))
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	expected := len("chunk1") + len("chunk2") + len("chunk3")
	if e.ResponseSize != expected {
		t.Errorf("expected response size %d, got %d", expected, e.ResponseSize)
	}
}

func TestResponseWriter_Unwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: inner, statusCode: 200}

	unwrapped := rw.Unwrap()
	if unwrapped != inner {
		t.Error("Unwrap() should return the inner ResponseWriter")
	}
}

// ─── Integration with stdlib mux ─────────────────────────────────────────────

func TestMiddleware_WorksWithServeMux(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"items":[]}`)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})

	wrapped := Middleware(c)(mux)

	// Request 1
	req1 := httptest.NewRequest("GET", "/api/items", nil)
	w1 := httptest.NewRecorder()
	wrapped.ServeHTTP(w1, req1)

	// Request 2
	req2 := httptest.NewRequest("GET", "/api/health", nil)
	w2 := httptest.NewRecorder()
	wrapped.ServeHTTP(w2, req2)

	if c.BufferLen() != 2 {
		t.Fatalf("expected 2 buffered events, got %d", c.BufferLen())
	}

	c.mu.Lock()
	e0 := c.buffer[0]
	e1 := c.buffer[1]
	c.mu.Unlock()

	if e0.Path != "/api/items" || e0.StatusCode != 200 {
		t.Errorf("event 0: expected /api/items 200, got %s %d", e0.Path, e0.StatusCode)
	}
	if e1.Path != "/api/health" || e1.StatusCode != 204 {
		t.Errorf("event 1: expected /api/health 204, got %s %d", e1.Path, e1.StatusCode)
	}
}

// ─── Request Size ────────────────────────────────────────────────────────────

func TestMiddleware_CapturesRequestSize(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	body := `{"key":"value"}`
	req := httptest.NewRequest("POST", "/api/data", strings.NewReader(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.mu.Lock()
	e := c.buffer[0]
	c.mu.Unlock()

	if e.RequestSize != len(body) {
		t.Errorf("expected request size %d, got %d", len(body), e.RequestSize)
	}
}

// ─── Crash Safety ────────────────────────────────────────────────────────────

func TestMiddleware_NilClient_PassesThrough(t *testing.T) {
	called := false
	handler := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("handler should have been called with nil client")
	}
	if w.Code != 200 {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestMiddleware_IdentifyConsumerPanic_DoesNotCrash(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c, err := New(Options{
		APIKey:        "ak_test_key",
		Endpoint:      ingestServer.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   tmpStoragePath(t),
		IdentifyConsumer: func(r *http.Request) string {
			panic("consumer callback exploded")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	called := false
	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
		w.Write([]byte("response body"))
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	w := httptest.NewRecorder()

	// Must NOT panic — the middleware recovers and the customer's response is served
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("handler should have been called despite IdentifyConsumer panic")
	}
	if w.Code != 200 {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "response body" {
		t.Errorf("expected body 'response body', got %q", w.Body.String())
	}
}

func TestMiddleware_CustomerHandlerSees200AfterRecovery(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c, err := New(Options{
		APIKey:        "ak_test_key",
		Endpoint:      ingestServer.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   tmpStoragePath(t),
		IdentifyConsumer: func(r *http.Request) string {
			panic("unexpected error in consumer callback")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":1}`))
	}))

	// Fire multiple requests — all must succeed
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/users", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != 201 {
			t.Errorf("request %d: expected status 201, got %d", i, w.Code)
		}
	}
}

// ─── CollectQueryString ──────────────────────────────────────────────────────

func TestMiddleware_CollectQueryString_Disabled(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c := newMiddlewareTestClient(t, ingestServer.URL)

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/search?z=3&a=1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.bufMu.Lock()
	e := c.buffer[0]
	c.bufMu.Unlock()

	if e.Path != "/search" {
		t.Errorf("expected path '/search', got '%s'", e.Path)
	}
}

func TestMiddleware_CollectQueryString_Enabled(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c, err := New(Options{
		APIKey:             "ak_test_key",
		Endpoint:           ingestServer.URL,
		FlushInterval:      1 * time.Hour,
		BatchSize:          1000,
		StoragePath:        tmpStoragePath(t),
		CollectQueryString: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/search?z=3&a=1", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.bufMu.Lock()
	e := c.buffer[0]
	c.bufMu.Unlock()

	if e.Path != "/search?a=1&z=3" {
		t.Errorf("expected path '/search?a=1&z=3', got '%s'", e.Path)
	}
}

func TestMiddleware_CollectQueryString_NoQueryString(t *testing.T) {
	ingestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ingestServer.Close()

	c, err := New(Options{
		APIKey:             "ak_test_key",
		Endpoint:           ingestServer.URL,
		FlushInterval:      1 * time.Hour,
		BatchSize:          1000,
		StoragePath:        tmpStoragePath(t),
		CollectQueryString: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(context.Background())

	handler := Middleware(c)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/users", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	c.bufMu.Lock()
	e := c.buffer[0]
	c.bufMu.Unlock()

	if e.Path != "/users" {
		t.Errorf("expected path '/users', got '%s'", e.Path)
	}
}
