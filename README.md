# API Usage Dashboard — Go SDK

Zero-dependency Go middleware for API analytics. Captures request metrics and sends them in batches to the API Usage Dashboard ingestion endpoint.

## Install

```bash
go get github.com/api-usage-dashboard/sdk-go
```

## Quick Start

```go
package main

import (
	"log"
	"net/http"

	apidash "github.com/api-usage-dashboard/sdk-go"
)

func main() {
	client, err := apidash.New(apidash.Options{
		APIKey:   "ak_your_api_key",
		Endpoint: "https://your-project.supabase.co/functions/v1/ingest",
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":"hello"}`))
	})

	handler := apidash.Middleware(client)(mux)
	log.Fatal(http.ListenAndServe(":8080", handler))
}
```

## Features

- **Zero dependencies** — only Go stdlib (`net/http`, `crypto/sha256`, `encoding/json`, etc.)
- **Buffered batching** — events are buffered in memory and flushed periodically or when the batch size is reached
- **Exponential backoff** — retries with 1s, 2s, 4s, 8s, 16s delays on failure
- **Disk persistence** — undelivered events are saved to a JSONL file and recovered on next startup
- **SSRF protection** — rejects private/loopback IP endpoints (except localhost for dev)
- **Consumer ID hashing** — Authorization headers are hashed (SHA-256) before storing
- **Graceful shutdown** — SIGTERM/SIGINT handlers persist buffered events to disk
- **Standard middleware** — `func(http.Handler) http.Handler` works with any Go router

## Options

| Field | Type | Default | Description |
|---|---|---|---|
| `APIKey` | `string` | required | API key for authentication |
| `Endpoint` | `string` | required | Ingestion endpoint URL |
| `FlushInterval` | `time.Duration` | `10s` | Time between automatic flushes |
| `BatchSize` | `int` | `100` | Events per batch (triggers flush) |
| `MaxBufferSize` | `int` | `10,000` | Max events in memory |
| `Debug` | `bool` | `false` | Enable debug logging |
| `IdentifyConsumer` | `func(*http.Request) string` | auto | Custom consumer ID extraction |
| `StoragePath` | `string` | temp dir | JSONL fallback file path |
| `MaxStorageBytes` | `int64` | `5MB` | Max storage file size |
| `TLSConfig` | `*tls.Config` | `nil` | Custom TLS configuration |

## Consumer Identification

By default, the middleware identifies API consumers by:

1. `X-API-Key` header — stored as-is
2. `Authorization` header — hashed with SHA-256 (prefix: `hash_`)

Override with a custom function:

```go
client, _ := apidash.New(apidash.Options{
	APIKey:   "ak_your_key",
	Endpoint: "https://example.com/ingest",
	IdentifyConsumer: func(r *http.Request) string {
		return r.Header.Get("X-Tenant-ID")
	},
})
```

## Graceful Shutdown

Use `Shutdown` with a context for timeout control:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := client.Shutdown(ctx); err != nil {
	log.Printf("shutdown error: %v", err)
}
```

## Testing

```bash
go test ./... -v
```
