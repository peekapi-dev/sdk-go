// Package peekapifiber provides a Fiber middleware adapter for the PeekAPI
// Go SDK. Unlike the Gin and Echo adapters, Fiber is built on
// fasthttp (not net/http), so this middleware calls client.Track() directly
// instead of using the shared TrackRequest helper.
package peekapifiber

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	peekapi "github.com/peekapi-dev/sdk-go"
)

// Options configures the Fiber middleware.
type Options struct {
	// IdentifyConsumer extracts a consumer identifier from a Fiber context.
	// If nil, the default strategy is used: X-API-Key header as-is, or hashed
	// Authorization header.
	IdentifyConsumer func(c *fiber.Ctx) string
}

// Middleware returns a Fiber middleware that captures request metrics and tracks
// them via the provided Client.
//
// If client is nil, the middleware is a no-op passthrough. Any panic during
// tracking is recovered — analytics must never break the customer's API.
func Middleware(client *peekapi.Client, opts ...Options) fiber.Handler {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	return func(c *fiber.Ctx) error {
		if client == nil {
			return c.Next()
		}

		start := time.Now()

		err := c.Next()

		trackFiber(client, c, opt, start)

		return err
	}
}

func trackFiber(client *peekapi.Client, c *fiber.Ctx, opt Options, start time.Time) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[peekapi] Panic in Fiber middleware (recovered): %v\n", rec)
		}
	}()

	duration := time.Since(start)

	consumerID := defaultFiberIdentifyConsumer(c)
	if opt.IdentifyConsumer != nil {
		consumerID = opt.IdentifyConsumer(c)
	}

	requestSize := len(c.Body())

	responseSize := len(c.Response().Body())

	client.Track(peekapi.RequestEvent{
		Method:         c.Method(),
		Path:           c.Path(),
		StatusCode:     c.Response().StatusCode(),
		ResponseTimeMs: math.Round(float64(duration.Microseconds())/10) / 100,
		RequestSize:    requestSize,
		ResponseSize:   responseSize,
		ConsumerID:     consumerID,
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// defaultFiberIdentifyConsumer mirrors the core SDK's DefaultIdentifyConsumer
// logic using Fiber's header accessors.
func defaultFiberIdentifyConsumer(c *fiber.Ctx) string {
	if key := c.Get("X-API-Key"); key != "" {
		return key
	}
	if auth := c.Get("Authorization"); auth != "" {
		return peekapi.HashConsumerID(auth)
	}
	return ""
}
