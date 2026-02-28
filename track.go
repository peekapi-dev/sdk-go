package peekapi

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// TrackRequest builds a RequestEvent from the given request/response metadata
// and tracks it via the Client. It encapsulates duration calculation, consumer
// identification, request size extraction, and timestamp generation.
//
// This method is used by the stdlib Middleware and the framework-specific
// middleware adapters (Gin, Echo) that wrap net/http under the hood.
//
// Any panic from IdentifyConsumer or Track is recovered — analytics must
// never break the customer's API.
func (c *Client) TrackRequest(r *http.Request, statusCode, responseSize int, start time.Time) {
	defer func() {
		if rec := recover(); rec != nil && c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[peekapi] Panic in TrackRequest (recovered): %v\n", rec)
		}
	}()

	duration := time.Since(start)
	consumerID := identifyConsumer(c.opts, r)

	requestSize := 0
	if r.ContentLength > 0 {
		requestSize = int(r.ContentLength)
	}

	path := r.URL.Path
	if c.opts.CollectQueryString && r.URL.RawQuery != "" {
		params := strings.Split(r.URL.RawQuery, "&")
		sort.Strings(params)
		path += "?" + strings.Join(params, "&")
	}

	c.Track(RequestEvent{
		Method:         r.Method,
		Path:           path,
		StatusCode:     statusCode,
		ResponseTimeMs: math.Round(float64(duration.Microseconds())/10) / 100,
		RequestSize:    requestSize,
		ResponseSize:   responseSize,
		ConsumerID:     consumerID,
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
	})
}
