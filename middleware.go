package peekapi

import (
	"net/http"
	"time"
)

// Middleware returns a standard net/http middleware that captures request
// metrics and tracks them via the provided Client. It works with any Go HTTP
// router that supports the func(http.Handler) http.Handler pattern (stdlib
// mux, chi, gorilla, etc.).
//
// Safety: if client is nil, the middleware is a no-op passthrough. Any panic
// during tracking (e.g. from a user-provided IdentifyConsumer callback) is
// recovered — analytics must never break the customer's API.
func Middleware(client *Client) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// Nil client → transparent passthrough, no overhead per request
		if client == nil {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, statusCode: 200}

			next.ServeHTTP(rw, r)

			client.TrackRequest(r, rw.statusCode, rw.bytesWritten, start)
		})
	}
}

func identifyConsumer(opts Options, r *http.Request) string {
	if opts.IdentifyConsumer != nil {
		return opts.IdentifyConsumer(r)
	}
	return DefaultIdentifyConsumer(r)
}

// responseWriter wraps http.ResponseWriter to capture the status code and
// response size.
type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	wroteHeader  bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

// Unwrap returns the underlying ResponseWriter for compatibility with
// http.ResponseController.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush implements http.Flusher if the underlying writer supports it.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
