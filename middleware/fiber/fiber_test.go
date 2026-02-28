package peekapifiber

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
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

	app := fiber.New()
	app.Use(Middleware(client))
	app.Post("/api/users", func(c *fiber.Ctx) error {
		c.Status(201)
		return c.SendString("created")
	})

	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", "15")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 201 {
		t.Fatalf("expected status 201, got %d", resp.StatusCode)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_DefaultStatus200(t *testing.T) {
	client, _ := newTestClient(t)

	app := fiber.New()
	app.Use(Middleware(client))
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/health", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_ConsumerFromAPIKey(t *testing.T) {
	client, _ := newTestClient(t)

	app := fiber.New()
	app.Use(Middleware(client))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendStatus(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-API-Key", "consumer-key-123")
	_, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_ConsumerHashesAuth(t *testing.T) {
	client, _ := newTestClient(t)

	app := fiber.New()
	app.Use(Middleware(client))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendStatus(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	_, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_CustomFiberIdentifyConsumer(t *testing.T) {
	client, _ := newTestClient(t)

	app := fiber.New()
	app.Use(Middleware(client, Options{
		IdentifyConsumer: func(c *fiber.Ctx) string {
			return c.Get("X-Tenant-ID")
		},
	}))
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendStatus(200)
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Tenant-ID", "tenant-42")
	_, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}

func TestMiddleware_NilClientPassthrough(t *testing.T) {
	app := fiber.New()
	app.Use(Middleware(nil))
	app.Get("/test", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected body 'ok', got %q", string(body))
	}
}

func TestMiddleware_PanicRecovery(t *testing.T) {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(context.Background())

	app := fiber.New()
	app.Use(Middleware(client, Options{
		IdentifyConsumer: func(c *fiber.Ctx) string {
			panic("consumer callback exploded")
		},
	}))
	app.Get("/test", func(c *fiber.Ctx) error {
		return c.SendString("response body")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "response body" {
		t.Errorf("expected body 'response body', got %q", string(body))
	}
}

func TestMiddleware_RequestSize(t *testing.T) {
	client, _ := newTestClient(t)

	app := fiber.New()
	app.Use(Middleware(client))
	app.Post("/api/data", func(c *fiber.Ctx) error {
		return c.SendStatus(200)
	})

	body := `{"key":"value"}`
	req := httptest.NewRequest("POST", "/api/data", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	_, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}

	if client.BufferLen() != 1 {
		t.Fatalf("expected 1 buffered event, got %d", client.BufferLen())
	}
}
