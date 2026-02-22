// Package apidash provides an API analytics SDK that captures HTTP request
// events, buffers them in memory, and flushes them in batches to an ingestion
// endpoint. It includes exponential backoff on failures, disk persistence for
// undelivered events, and SSRF protection.
package apidash

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultFlushInterval   = 10 * time.Second
	defaultBatchSize       = 100
	defaultMaxBufferSize   = 10_000
	maxPathLength          = 2048
	maxMethodLength        = 16
	maxConsumerIDLength    = 256
	maxConsecutiveFailures = 5
	baseBackoff            = 1 * time.Second
	defaultMaxStorageBytes = 5 * 1024 * 1024 // 5MB
	sendTimeout            = 5 * time.Second
	dnsCacheTTL            = 60 * time.Second
)

// ErrEventsPersisted indicates that Shutdown could not deliver events to the
// ingestion endpoint and they were persisted to the local disk file instead.
// Callers can check for this with errors.Is(err, ErrEventsPersisted).
var ErrEventsPersisted = fmt.Errorf("apidash: events persisted to disk, not delivered to endpoint")

// jsonBufPool reuses bytes.Buffer instances for JSON marshaling in send(),
// reducing GC pressure under high flush throughput.
var jsonBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// privateIPRe matches private, loopback, and link-local IP ranges for SSRF protection.
// Used for fast hostname-string checks at construction time.
var privateIPRe = regexp.MustCompile(
	`^(127\.|10\.|172\.(1[6-9]|2\d|3[01])\.|192\.168\.|169\.254\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|0\.|::1$|fc|fd|fe80|::ffff:)`,
)

// isPrivateIPAddr checks whether a net.IP is in a private/reserved range.
// Handles IPv4, IPv6, and IPv4-mapped IPv6 addresses numerically.
func isPrivateIPAddr(ip net.IP) bool {
	// Normalize IPv4-mapped IPv6 (e.g. ::ffff:10.0.0.1) to IPv4
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	// Check against well-known private ranges
	privateRanges := []struct {
		network string
		cidr    string
	}{
		{"0.0.0.0", "0.0.0.0/8"},          // Current network
		{"10.0.0.0", "10.0.0.0/8"},        // RFC 1918
		{"100.64.0.0", "100.64.0.0/10"},   // CGNAT (RFC 6598)
		{"127.0.0.0", "127.0.0.0/8"},      // Loopback
		{"169.254.0.0", "169.254.0.0/16"}, // Link-local
		{"172.16.0.0", "172.16.0.0/12"},   // RFC 1918
		{"192.168.0.0", "192.168.0.0/16"}, // RFC 1918
	}
	for _, r := range privateRanges {
		_, network, _ := net.ParseCIDR(r.cidr)
		if network.Contains(ip) {
			return true
		}
	}

	// IPv6-specific checks
	if len(ip) == net.IPv6len {
		// ::1 (loopback)
		if ip.Equal(net.IPv6loopback) {
			return true
		}
		// fc00::/7 (ULA)
		if ip[0]&0xfe == 0xfc {
			return true
		}
		// fe80::/10 (link-local)
		if ip[0] == 0xfe && ip[1]&0xc0 == 0x80 {
			return true
		}
	}

	return false
}

// Options configures the API dashboard client.
type Options struct {
	// APIKey is the API key used to authenticate with the ingestion endpoint (required).
	APIKey string

	// Endpoint is the URL of the ingestion endpoint (required).
	Endpoint string

	// FlushInterval is the time between automatic flushes. Default: 10s.
	FlushInterval time.Duration

	// BatchSize is the number of events that triggers an automatic flush. Default: 100.
	BatchSize int

	// MaxBufferSize is the maximum number of events held in memory. Default: 10,000.
	MaxBufferSize int

	// Debug enables debug logging to stderr.
	Debug bool

	// IdentifyConsumer is an optional function to extract a consumer identifier
	// from an HTTP request. If nil, the default logic uses X-API-Key or hashes
	// the Authorization header.
	IdentifyConsumer func(r *http.Request) string

	// StoragePath is the file path for persisting undelivered events.
	// Default: os.TempDir()/apidash-events-<hash>.jsonl
	StoragePath string

	// MaxStorageBytes is the maximum size of the storage file. Default: 5MB.
	MaxStorageBytes int64

	// TLSConfig is an optional TLS configuration for the HTTP client.
	TLSConfig *tls.Config

	// OnError is an optional callback invoked when the background flush loop
	// encounters an error (network failure, non-retryable status, etc.).
	// Called from the background goroutine — implementations must be safe for
	// concurrent use and should not block.
	OnError func(err error)
}

// RequestEvent represents a single captured API request.
type RequestEvent struct {
	Method         string                 `json:"method"`
	Path           string                 `json:"path"`
	StatusCode     int                    `json:"status_code"`
	ResponseTimeMs float64                `json:"response_time_ms"`
	RequestSize    int                    `json:"request_size"`
	ResponseSize   int                    `json:"response_size"`
	ConsumerID     string                 `json:"consumer_id,omitempty"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
	Timestamp      string                 `json:"timestamp"`
}

// Client buffers request events and sends them to an ingestion endpoint.
type Client struct {
	mu    sync.Mutex // protects flush state: flushInFlight, consecutiveFailures, backoffUntil, closed
	bufMu sync.Mutex // protects buffer and spare (hot path — only lock needed by Track)

	opts                Options
	parsedURL           *url.URL
	httpClient          *http.Client
	buffer              []RequestEvent
	spare               []RequestEvent // double-buffer: pre-allocated spare for swap in FlushContext
	flushInFlight       bool
	consecutiveFailures int
	backoffUntil        time.Time
	storagePath         string
	maxStorageBytes     int64

	flushCh chan struct{} // non-blocking flush trigger
	done    chan struct{} // signals background goroutine to stop
	stopped chan struct{} // closed when background goroutine exits
	closed  bool

	signalCh chan os.Signal // for graceful shutdown
}

// New creates a new Client with the given options. It validates the
// configuration, loads any previously persisted events from disk, and starts
// a background goroutine for periodic flushing.
func New(opts Options) (*Client, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("[apidash] 'Endpoint' is required")
	}

	parsed, err := url.Parse(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("[apidash] Invalid endpoint URL: %s", opts.Endpoint)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("[apidash] Invalid endpoint URL: %s", opts.Endpoint)
	}

	hostname := parsed.Hostname()
	isLocalhost := hostname == "localhost" || hostname == "127.0.0.1"

	if parsed.Scheme != "https" && !isLocalhost {
		return nil, fmt.Errorf("[apidash] Endpoint must use HTTPS. Plain HTTP is only allowed for localhost.")
	}

	if !isLocalhost && privateIPRe.MatchString(hostname) {
		return nil, fmt.Errorf("[apidash] Endpoint must not point to a private or internal IP address.")
	}

	// Strip embedded credentials
	if parsed.User != nil {
		parsed.User = nil
		if opts.Debug {
			fmt.Fprintln(os.Stderr, "[apidash] Stripped embedded credentials from endpoint URL")
		}
	}

	if opts.APIKey == "" {
		return nil, fmt.Errorf("[apidash] 'APIKey' is required")
	}
	if strings.ContainsAny(opts.APIKey, "\r\n\x00") {
		return nil, fmt.Errorf("[apidash] 'APIKey' contains invalid characters")
	}

	// Apply defaults
	if opts.FlushInterval == 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.MaxBufferSize == 0 {
		opts.MaxBufferSize = defaultMaxBufferSize
	}
	if opts.MaxStorageBytes == 0 {
		opts.MaxStorageBytes = defaultMaxStorageBytes
	}

	storagePath := opts.StoragePath
	if storagePath == "" {
		h := md5.Sum([]byte(opts.Endpoint))
		storagePath = filepath.Join(os.TempDir(), fmt.Sprintf("apidash-events-%s.jsonl", hex.EncodeToString(h[:4])))
	}

	dialer := &net.Dialer{Timeout: 3 * time.Second}

	// Per-client DNS cache with TTL. Eliminates per-socket DNS lookups while
	// still re-validating IPs on cache miss (SSRF protection).
	var dnsMu sync.Mutex
	type dnsCacheEntry struct {
		ips     []string
		expires time.Time
	}
	dnsCache := make(map[string]dnsCacheEntry)

	// SSRF-safe DialContext: validates all resolved IPs before connecting.
	// Prevents DNS rebinding attacks where a hostname initially resolves to a
	// public IP (passing construction-time checks) then later resolves to a
	// private IP. Skipped for localhost since private IPs are expected there.
	ssrfSafeDialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		// Skip check for localhost (allowed for local dev)
		if host == "localhost" || host == "127.0.0.1" {
			return dialer.DialContext(ctx, network, addr)
		}

		// Check DNS cache
		dnsMu.Lock()
		cached, ok := dnsCache[host]
		if ok && time.Now().Before(cached.expires) {
			dnsMu.Unlock()
			return dialer.DialContext(ctx, network, net.JoinHostPort(cached.ips[0], port))
		}
		dnsMu.Unlock()

		// Resolve all IPs and validate each one
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}

		for _, ipStr := range ips {
			ip := net.ParseIP(ipStr)
			if ip != nil && isPrivateIPAddr(ip) {
				return nil, fmt.Errorf("[apidash] DNS resolved to private IP %s (SSRF protection)", ipStr)
			}
		}

		// Cache the validated result
		dnsMu.Lock()
		dnsCache[host] = dnsCacheEntry{ips: ips, expires: time.Now().Add(dnsCacheTTL)}
		dnsMu.Unlock()

		// Connect using the first validated IP
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
	}

	transport := &http.Transport{
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		DialContext:           ssrfSafeDialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
	}
	if opts.TLSConfig != nil {
		transport.TLSClientConfig = opts.TLSConfig
	}

	c := &Client{
		opts:            opts,
		parsedURL:       parsed,
		storagePath:     storagePath,
		maxStorageBytes: opts.MaxStorageBytes,
		httpClient: &http.Client{
			Timeout:   sendTimeout,
			Transport: transport,
		},
		buffer:  make([]RequestEvent, 0, opts.BatchSize),
		spare:   make([]RequestEvent, 0, opts.BatchSize),
		flushCh: make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	c.loadFromDisk()
	go c.backgroundLoop()
	c.registerSignalHandlers()

	return c, nil
}

// Track adds an event to the buffer. If the buffer reaches BatchSize, a
// non-blocking flush is triggered. Track is safe for concurrent use.
func (c *Client) Track(event RequestEvent) {
	// Sanitize input lengths
	if len(event.Method) > maxMethodLength {
		event.Method = event.Method[:maxMethodLength]
	}
	if len(event.Path) > maxPathLength {
		event.Path = event.Path[:maxPathLength]
	}
	if len(event.ConsumerID) > maxConsumerIDLength {
		event.ConsumerID = event.ConsumerID[:maxConsumerIDLength]
	}

	c.bufMu.Lock()
	c.buffer = append(c.buffer, event)
	// Trigger flush when buffer reaches batchSize OR maxBufferSize.
	// At maxBufferSize the flush drains events to the server, preventing
	// the buffer from growing without bound. If the endpoint is down,
	// the existing retry + backoff + disk-persist-after-max-failures
	// mechanism kicks in.
	shouldFlush := len(c.buffer) >= c.opts.BatchSize
	c.bufMu.Unlock()

	if shouldFlush {
		// Non-blocking send to flush channel
		select {
		case c.flushCh <- struct{}{}:
		default:
		}
	}
}

// Flush sends all buffered events to the ingestion endpoint. It respects
// in-flight and backoff guards. Flush is safe for concurrent use.
// Equivalent to FlushContext(context.Background()).
func (c *Client) Flush() error {
	return c.FlushContext(context.Background())
}

// FlushContext sends all buffered events to the ingestion endpoint with
// context support for cancellation and timeouts. It respects in-flight
// and backoff guards. FlushContext is safe for concurrent use.
//
// Locking strategy: mu protects flush state (flushInFlight, backoff),
// bufMu protects buffer/spare. Track() only acquires bufMu, so it never
// blocks on flush state or network I/O. Lock ordering: mu → bufMu.
func (c *Client) FlushContext(ctx context.Context) error {
	c.mu.Lock()
	if c.flushInFlight {
		c.mu.Unlock()
		return nil
	}
	if c.consecutiveFailures > 0 && time.Now().Before(c.backoffUntil) {
		c.mu.Unlock()
		return nil
	}
	c.flushInFlight = true
	c.mu.Unlock()

	// Swap buffer under bufMu only (brief lock — Track() contention is minimal)
	c.bufMu.Lock()
	if len(c.buffer) == 0 {
		c.bufMu.Unlock()
		c.mu.Lock()
		c.flushInFlight = false
		c.mu.Unlock()
		return nil
	}
	events := c.buffer
	// Double-buffer swap: reuse the pre-allocated spare slice instead of
	// allocating a new one on every flush. The spare is reset to zero length
	// but keeps its backing array from the previous cycle.
	c.buffer = c.spare[:0]
	c.spare = nil
	c.bufMu.Unlock()

	// Network I/O — no locks held
	err := c.send(ctx, events)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.flushInFlight = false

	// recycleSpare returns the events slice to the spare pool under bufMu.
	recycleSpare := func() {
		c.bufMu.Lock()
		if c.spare == nil {
			c.spare = events[:0]
		}
		c.bufMu.Unlock()
	}

	if err == nil {
		c.consecutiveFailures = 0
		c.backoffUntil = time.Time{}
		recycleSpare()
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Flushed %d events\n", len(events))
		}
		return nil
	}

	if !isSendRetryable(err) {
		// Non-retryable (4xx) — persist to disk, don't waste retry budget
		c.persistToDisk(events)
		recycleSpare()
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Non-retryable error, persisted to disk: %v\n", err)
		}
		return err
	}

	c.consecutiveFailures++

	if c.consecutiveFailures >= maxConsecutiveFailures {
		c.persistToDisk(events)
		recycleSpare()
		c.consecutiveFailures = 0
	} else {
		// Re-insert events at the front of the buffer under bufMu.
		// Build into a single pre-sized slice using copy to avoid the O(n)
		// prepend allocation of append(events, c.buffer...).
		c.bufMu.Lock()
		capacity := c.opts.MaxBufferSize - len(c.buffer)
		if capacity > len(events) {
			capacity = len(events)
		}
		if capacity > 0 {
			merged := make([]RequestEvent, capacity+len(c.buffer))
			copy(merged, events[:capacity])
			copy(merged[capacity:], c.buffer)
			c.buffer = merged
		}
		// Recycle events slice as spare (already holding bufMu)
		if c.spare == nil {
			c.spare = events[:0]
		}
		c.bufMu.Unlock()
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Flush failed (attempt %d/%d): %v\n",
				c.consecutiveFailures, maxConsecutiveFailures, err)
		}
	}

	// Exponential backoff with jitter: base * 2^(n-1) * random(0.5..1.0)
	if c.consecutiveFailures > 0 {
		base := baseBackoff * (1 << (c.consecutiveFailures - 1))
		jitter := 0.5 + rand.Float64()*0.5
		c.backoffUntil = time.Now().Add(time.Duration(float64(base) * jitter))
	}
	return err
}

// Shutdown gracefully stops the client: stops the background flush goroutine,
// flushes remaining events, and persists any undelivered events to disk.
// The provided context controls the shutdown timeout.
//
// Returns ErrEventsPersisted if events could not be delivered to the ingestion
// endpoint and were persisted to the local disk file instead.
func (c *Client) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// Stop signal handler
	if c.signalCh != nil {
		signal.Stop(c.signalCh)
	}

	// Stop background loop
	close(c.done)

	// Wait for background goroutine to exit (with context timeout)
	select {
	case <-c.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Flush remaining buffer using the shutdown context
	c.mu.Lock()
	c.flushInFlight = false // reset so flush can proceed
	c.mu.Unlock()

	flushErr := c.FlushContext(ctx)

	// If buffer still has events (flush failed or re-inserted), persist to disk
	c.bufMu.Lock()
	remaining := len(c.buffer)
	if remaining > 0 {
		events := c.buffer
		c.buffer = nil
		c.bufMu.Unlock()
		c.persistToDisk(events)
		c.httpClient.CloseIdleConnections()
		return ErrEventsPersisted
	}
	c.bufMu.Unlock()

	c.httpClient.CloseIdleConnections()

	// FlushContext may have persisted events internally (non-retryable error
	// or max consecutive failures reached) — report that to the caller.
	if flushErr != nil {
		return ErrEventsPersisted
	}
	return nil
}

// BufferLen returns the current number of events in the buffer (for testing).
func (c *Client) BufferLen() int {
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	return len(c.buffer)
}

func (c *Client) backgroundLoop() {
	defer close(c.stopped)
	ticker := time.NewTicker(c.opts.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.Flush(); err != nil && c.opts.OnError != nil {
				c.opts.OnError(err)
			}
		case <-c.flushCh:
			if err := c.Flush(); err != nil && c.opts.OnError != nil {
				c.opts.OnError(err)
			}
		case <-c.done:
			return
		}
	}
}

func (c *Client) registerSignalHandlers() {
	c.signalCh = make(chan os.Signal, 1)
	signal.Notify(c.signalCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		select {
		case <-c.signalCh:
			// NOTE: The SDK no longer calls os.Exit() — it only persists
			// buffered events to disk and lets the host application control
			// its own lifecycle.
			c.shutdownSync()
		case <-c.done:
			return
		}
	}()
}

// shutdownSync is the synchronous path used by signal handlers.
// It attempts a quick network flush (2s timeout) before falling back to
// disk persistence, so events are delivered when the endpoint is healthy.
func (c *Client) shutdownSync() {
	c.bufMu.Lock()
	if len(c.buffer) == 0 {
		c.bufMu.Unlock()
		c.httpClient.CloseIdleConnections()
		return
	}
	c.bufMu.Unlock()

	// Reset flushInFlight so FlushContext can proceed
	c.mu.Lock()
	c.flushInFlight = false
	c.mu.Unlock()

	// Attempt a quick network flush with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.FlushContext(ctx)

	// Persist anything that didn't make it to the network
	c.bufMu.Lock()
	if len(c.buffer) > 0 {
		events := c.buffer
		c.buffer = nil
		c.bufMu.Unlock()
		c.persistToDisk(events)
	} else {
		c.bufMu.Unlock()
	}
	c.httpClient.CloseIdleConnections()
}

// persistToDisk writes events to the storage file in JSONL format (one JSON
// array per line). Only accesses immutable fields (storagePath, maxStorageBytes)
// so no lock is required.
func (c *Client) persistToDisk(events []RequestEvent) {
	if len(events) == 0 {
		return
	}

	// Check file size before writing
	var currentSize int64
	if info, err := os.Stat(c.storagePath); err == nil {
		currentSize = info.Size()
	}
	if currentSize >= c.maxStorageBytes {
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Storage file full (%d bytes), skipping disk persist of %d events\n",
				currentSize, len(events))
		}
		return
	}

	data, err := json.Marshal(events)
	if err != nil {
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Failed to marshal events for disk: %v\n", err)
		}
		return
	}

	f, err := os.OpenFile(c.storagePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Failed to open storage file: %v\n", err)
		}
		return
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		if c.opts.Debug {
			fmt.Fprintf(os.Stderr, "[apidash] Failed to write events to disk: %v\n", err)
		}
	} else if c.opts.Debug {
		fmt.Fprintf(os.Stderr, "[apidash] Persisted %d events to %s\n", len(events), c.storagePath)
	}
}

// loadFromDisk reads persisted events from disk back into the buffer.
func (c *Client) loadFromDisk() {
	f, err := os.Open(c.storagePath)
	if err != nil {
		return // file doesn't exist or can't be read
	}
	defer f.Close()

	loaded := 0
	scanner := bufio.NewScanner(f)
	// Increase scanner buffer for potentially large lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var batch []RequestEvent
		if err := json.Unmarshal([]byte(line), &batch); err != nil {
			continue // skip corrupt lines
		}
		for _, event := range batch {
			if len(c.buffer) >= c.opts.MaxBufferSize {
				break
			}
			c.buffer = append(c.buffer, event)
			loaded++
		}
		if len(c.buffer) >= c.opts.MaxBufferSize {
			break
		}
	}

	f.Close() // close before removing
	os.Remove(c.storagePath)

	if c.opts.Debug && loaded > 0 {
		fmt.Fprintf(os.Stderr, "[apidash] Recovered %d events from disk\n", loaded)
	}
}

func (c *Client) send(ctx context.Context, events []RequestEvent) error {
	buf := jsonBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonBufPool.Put(buf)

	if err := json.NewEncoder(buf).Encode(events); err != nil {
		return fmt.Errorf("failed to marshal events: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.parsedURL.String(), buf)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.opts.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		// Limit read to 1KB to avoid blocking on large error pages
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == 429 || resp.StatusCode >= 500
		return &sendError{statusCode: resp.StatusCode, retryable: retryable}
	}
	return nil
}

// sendError represents an ingestion API error with retryability info.
type sendError struct {
	statusCode int
	retryable  bool
}

func (e *sendError) Error() string {
	return fmt.Sprintf("ingestion API returned %d", e.statusCode)
}

func isSendRetryable(err error) bool {
	if se, ok := err.(*sendError); ok {
		return se.retryable
	}
	// Network errors are retryable
	return true
}

// HashConsumerID hashes a raw consumer identifier (e.g. Authorization header)
// to a short, stable, non-reversible identifier using SHA-256.
func HashConsumerID(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return "hash_" + hex.EncodeToString(h[:6]) // 12 hex chars
}

// DefaultIdentifyConsumer extracts a consumer ID from the request using the
// default strategy: X-API-Key header as-is, or hashed Authorization header.
func DefaultIdentifyConsumer(r *http.Request) string {
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		return HashConsumerID(auth)
	}
	return ""
}

// IsPrivateIP reports whether the given host string matches a private,
// loopback, or link-local IP address pattern. It checks both the string
// representation (regex) and the numeric value (net.IP parsing).
func IsPrivateIP(host string) bool {
	if privateIPRe.MatchString(host) {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return isPrivateIPAddr(ip)
	}
	return false
}

// ResolveHost resolves a hostname to its first IP address. Exported for testing.
func ResolveHost(host string) (string, error) {
	ips, err := net.LookupHost(host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no IPs found for host: %s", host)
	}
	return ips[0], nil
}
