# PeekAPI — Go SDK

[![Go Reference](https://pkg.go.dev/badge/github.com/peekapi-dev/sdk-go.svg)](https://pkg.go.dev/github.com/peekapi-dev/sdk-go)
[![license](https://img.shields.io/github/license/peekapi-dev/sdk-go)](./LICENSE)
[![CI](https://github.com/peekapi-dev/sdk-go/actions/workflows/ci.yml/badge.svg)](https://github.com/peekapi-dev/sdk-go/actions/workflows/ci.yml)

Zero-dependency Go SDK for [PeekAPI](https://peekapi.dev). Standard `net/http` middleware plus adapters for Gin, Echo, Chi, and Fiber.

## Install

```bash
go get github.com/peekapi-dev/sdk-go
```

## Quick Start

```go
package main

import (
	"log"
	"net/http"

	peekapi "github.com/peekapi-dev/sdk-go"
)

func main() {
	client, err := peekapi.New(peekapi.Options{
		APIKey: "ak_live_xxx",
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":"hello"}`))
	})

	handler := peekapi.Middleware(client)(mux)
	log.Fatal(http.ListenAndServe(":8080", handler))
}
```

## Framework Support

The core middleware uses the standard `func(http.Handler) http.Handler` signature. Framework-specific adapters are available as sub-packages:

| Framework | Package | Install |
|---|---|---|
| net/http, Chi | `peekapi` (core) | `go get github.com/peekapi-dev/sdk-go` |
| Gin | `peekapigin` | `go get github.com/peekapi-dev/sdk-go/middleware/gin` |
| Echo | `peekapiecho` | `go get github.com/peekapi-dev/sdk-go/middleware/echo` |
| Fiber | `peekapifiber` | `go get github.com/peekapi-dev/sdk-go/middleware/fiber` |

### Chi

Chi uses the standard middleware signature natively — no extra package needed:

```go
r := chi.NewRouter()
r.Use(peekapi.Middleware(client))
```

### Gin

```go
import peekapigin "github.com/peekapi-dev/sdk-go/middleware/gin"

engine := gin.Default()
engine.Use(peekapigin.Middleware(client))
```

### Echo

```go
import peekapiecho "github.com/peekapi-dev/sdk-go/middleware/echo"

e := echo.New()
e.Use(peekapiecho.Middleware(client))
```

### Fiber

```go
import peekapifiber "github.com/peekapi-dev/sdk-go/middleware/fiber"

app := fiber.New()
app.Use(peekapifiber.Middleware(client))

// With custom consumer identification
app.Use(peekapifiber.Middleware(client, peekapifiber.Options{
	IdentifyConsumer: func(c *fiber.Ctx) string {
		return c.Get("X-Tenant-ID")
	},
}))
```

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `APIKey` | `string` | required | Your PeekAPI key |
| `Endpoint` | `string` | PeekAPI cloud | Ingestion endpoint URL |
| `FlushInterval` | `time.Duration` | `10s` | Time between automatic flushes |
| `BatchSize` | `int` | `100` | Events per batch (triggers flush) |
| `MaxBufferSize` | `int` | `10,000` | Max events held in memory |
| `MaxStorageBytes` | `int64` | `5MB` | Max disk fallback file size |
| `Debug` | `bool` | `false` | Enable debug logging |
| `IdentifyConsumer` | `func(*http.Request) string` | auto | Custom consumer ID extraction |
| `StoragePath` | `string` | temp dir | JSONL fallback file path |
| `TLSConfig` | `*tls.Config` | `nil` | Custom TLS configuration |

## How It Works

1. Middleware intercepts every request/response
2. Captures method, path, status code, response time, request/response sizes, consumer ID
3. Events are buffered in memory and flushed in batches on a background goroutine
4. On network failure: exponential backoff with jitter (1s, 2s, 4s, 8s, 16s)
5. After max retries: events are persisted to a JSONL file on disk
6. On next startup: persisted events are recovered and re-sent
7. On SIGTERM/SIGINT: remaining buffer is flushed or persisted to disk

## Consumer Identification

By default, consumers are identified by:

1. `X-API-Key` header — stored as-is
2. `Authorization` header — hashed with SHA-256 (stored as `hash_<hex>`)

Override with the `IdentifyConsumer` option:

```go
client, _ := peekapi.New(peekapi.Options{
	APIKey: "ak_live_xxx",
	IdentifyConsumer: func(r *http.Request) string {
		return r.Header.Get("X-Tenant-ID")
	},
})
```

## Graceful Shutdown

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := client.Shutdown(ctx); err != nil {
	log.Printf("shutdown error: %v", err)
}
```

## Features

- **Zero dependencies** — only Go stdlib
- **Buffered batching** — in-memory buffer with configurable flush interval and batch size
- **Exponential backoff** — retries with jitter on network failures
- **Disk persistence** — undelivered events saved to JSONL, recovered on restart
- **SSRF protection** — rejects private/loopback IP endpoints (except localhost)
- **Consumer ID hashing** — Authorization headers hashed with SHA-256
- **Graceful shutdown** — context-based with SIGTERM/SIGINT handlers
- **Standard middleware** — `func(http.Handler) http.Handler` works with any router

## Contributing

Bug reports and feature requests: [peekapi-dev/community](https://github.com/peekapi-dev/community/issues)

1. Fork & clone the repo
2. Run tests — `go test ./...`
3. Lint — `go vet ./...`
4. Format — `gofmt -w .`
5. Submit a PR

## License

MIT
