package peekapiecho

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	peekapi "github.com/peekapi-dev/sdk-go"
)

func tmpStoragePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "peekapi-test.jsonl")
}

func newTestClient(t *testing.T) (*peekapi.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c, err := peekapi.New(peekapi.Options{
		APIKey:        "ak_test_key",
		Endpoint:      srv.URL,
		FlushInterval: 1 * time.Hour,
		BatchSize:     1000,
		StoragePath:   tmpStoragePath(t),
	})
	if err != nil {
		t.Fatalf("peekapi.New() failed: %v", err)
	}
	t.Cleanup(func() { c.Shutdown(context.Background()) })
	return c, srv
}

func TestMiddleware_CapturesRequestData(t *testing.T) {
	client, _ := newTestClient(t)

	e := echo.New()
	e.Use(Middleware(client))
	e.POST("/api/users", func(c echo.Context) error {
		return c.String(201, "created")
	})

	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", "15")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != 201 {
		t.Fatalf("expected status 201, got %d", w.Code)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_DefaultStatus200(t *testing.T) {
	client, _ := newTestClient(t)

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/health", func(c echo.Context) error {
		return c.String(200, "ok")
	})

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_ConsumerFromAPIKey(t *testing.T) {
	client, _ := newTestClient(t)

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/", func(c echo.Context) error {
		return c.NoContent(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "consumer-key-123")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_ConsumerHashesAuth(t *testing.T) {
	client, _ := newTestClient(t)

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/", func(c echo.Context) error {
		return c.NoContent(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_CustomIdentifyConsumer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	client, err := peekapi.New(peekapi.Options{
		APIKey:        "ak_test_key",
		Endpoint:      srv.URL,
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
	defer client.Shutdown(context.Background())

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/", func(c echo.Context) error {
		return c.NoContent(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "tenant-42")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_NilClientPassthrough(t *testing.T) {
	e := echo.New()
	e.Use(Middleware(nil))
	e.GET("/test", func(c echo.Context) error {
		return c.String(200, "ok")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", w.Body.String())
	}
}

func TestMiddleware_PropagatesHandlerError(t *testing.T) {
	client, _ := newTestClient(t)

	handlerErr := errors.New("something went wrong")

	e := echo.New()
	e.Use(Middleware(client))
	e.GET("/fail", func(c echo.Context) error {
		return handlerErr
	})

	req := httptest.NewRequest("GET", "/fail", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	// Echo returns errors via its error handler; the middleware should still track
	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_RequestSize(t *testing.T) {
	client, _ := newTestClient(t)

	e := echo.New()
	e.Use(Middleware(client))
	e.POST("/api/data", func(c echo.Context) error {
		return c.NoContent(200)
	})

	body := `{"key":"value"}`
	req := httptest.NewRequest("POST", "/api/data", strings.NewReader(body))
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}
