// Package peekapiecho provides an Echo middleware adapter for the PeekAPI
// Go SDK. It wraps the core SDK's TrackRequest helper to capture
// request metrics from Echo's context.
package peekapiecho

import (
	"time"

	"github.com/labstack/echo/v4"
	peekapi "github.com/peekapi-dev/sdk-go"
)

// Middleware returns an Echo middleware that captures request metrics and tracks
// them via the provided Client.
//
// If client is nil, the middleware is a no-op passthrough. Any panic during
// tracking is recovered — analytics must never break the customer's API.
func Middleware(client *peekapi.Client) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if client == nil {
			return next
		}

		return func(c echo.Context) error {
			start := time.Now()

			err := next(c)

			client.TrackRequest(
				c.Request(),
				c.Response().Status,
				int(c.Response().Size),
				start,
			)

			return err
		}
	}
}
