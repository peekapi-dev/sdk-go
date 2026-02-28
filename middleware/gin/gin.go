// Package peekapigin provides a Gin middleware adapter for the PeekAPI
// Go SDK. It wraps the core SDK's TrackRequest helper to capture
// request metrics from Gin's context.
package peekapigin

import (
	"time"

	"github.com/gin-gonic/gin"
	peekapi "github.com/peekapi-dev/sdk-go"
)

// Middleware returns a Gin middleware that captures request metrics and tracks
// them via the provided Client.
//
// If client is nil, the middleware is a no-op passthrough. Any panic during
// tracking is recovered — analytics must never break the customer's API.
func Middleware(client *peekapi.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		if client == nil {
			c.Next()
			return
		}

		start := time.Now()

		c.Next()

		// c.Writer.Size() returns -1 if nothing was written
		responseSize := c.Writer.Size()
		if responseSize < 0 {
			responseSize = 0
		}

		client.TrackRequest(c.Request, c.Writer.Status(), responseSize, start)
	}
}
