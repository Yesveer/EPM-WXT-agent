// Package tunnel implements the wxt-agent side of the vsay-tunnel protocol.
//
// How it works (ngrok-style reverse tunnel):
//
//  1. Client polls GET {TUNNEL_URL}/agent/tunnels?machine_id=XXX every poll_interval.
//  2. For each pending/active tunnel not yet connected, Client opens a WebSocket to
//     {TUNNEL_URL}/agent/ws?tunnel_id=…&machine_id=…&token=…
//  3. vsay-tunnel sends HTTP request messages over that WebSocket.
//  4. Client dials localhost:{local_port}, forwards the request, and sends the response back.
//  5. If the WebSocket drops, Client retries with exponential back-off.
package tunnel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// ── Protocol types (must match vsay-tunnel/internal/tunnel/manager.go) ────────

const (
	typeConnected = "connected"
	typeRequest   = "request"
	typeResponse  = "response"
)

type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type tunnelInfo struct {
	TunnelID   string `json:"tunnel_id"`
	Subdomain  string `json:"subdomain"`
	LocalPort  int    `json:"local_port"`
	PublicURL  string `json:"public_url"`
	AgentToken string `json:"agent_token"`
}

type tunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64-encoded
}

type tunnelResponse struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"` // base64-encoded
}

// hop-by-hop headers stripped from forwarded requests and responses
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailers",
	"Transfer-Encoding", "Upgrade",
}

// ── Client ───────────────────────────────────────────────────────────────────

// Client manages tunnel connections for a single machine.
type Client struct {
	baseURL      string // e.g. "http://localhost:8083" — no trailing slash
	machineID    string
	pollInterval time.Duration
	logger       *zap.Logger

	mu     sync.Mutex
	active map[string]context.CancelFunc // tunnelID → cancel func
}

// New creates a tunnel Client.
//
//	baseURL      — vsay-tunnel management API base (e.g. "http://vsay-tunnel:8083")
//	machineID    — this agent's machine/agent ID (used to query tunnels)
//	pollInterval — how often to poll for new tunnels (e.g. 10s)
func New(baseURL, machineID string, pollInterval time.Duration, logger *zap.Logger) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		machineID:    machineID,
		pollInterval: pollInterval,
		logger:       logger,
		active:       make(map[string]context.CancelFunc),
	}
}

// Start begins polling vsay-tunnel and serving any assigned tunnels.
// It blocks until ctx is cancelled.
func (c *Client) Start(ctx context.Context) {
	c.logger.Info("Tunnel client started",
		zap.String("tunnel_url", c.baseURL),
		zap.String("machine_id", c.machineID),
		zap.Duration("poll_interval", c.pollInterval))

	// Poll immediately, then on every tick
	c.poll(ctx)

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

// poll fetches the list of tunnels this machine should serve and starts
// WebSocket goroutines for any that are not yet connected.
func (c *Client) poll(ctx context.Context) {
	reqURL := fmt.Sprintf("%s/agent/tunnels?machine_id=%s", c.baseURL, url.QueryEscape(c.machineID))
	resp, err := http.Get(reqURL) // #nosec G107 -- baseURL is agent config, machineID is URL-escaped above
	if err != nil {
		c.logger.Debug("Tunnel poll error", zap.Error(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Debug("Tunnel poll non-200", zap.Int("status", resp.StatusCode))
		return
	}

	var body struct {
		Tunnels []tunnelInfo `json:"tunnels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return
	}

	for _, t := range body.Tunnels {
		c.mu.Lock()
		_, running := c.active[t.TunnelID]
		c.mu.Unlock()
		if running {
			continue
		}

		tunnelCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.active[t.TunnelID] = cancel
		c.mu.Unlock()

		go c.serveTunnel(tunnelCtx, t, cancel)
	}
}

// serveTunnel runs the connect-retry loop for a single tunnel.
// It exits when ctx is cancelled or the tunnel is removed from vsay-tunnel.
func (c *Client) serveTunnel(ctx context.Context, t tunnelInfo, cancel context.CancelFunc) {
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.active, t.TunnelID)
		c.mu.Unlock()
		c.logger.Info("Tunnel goroutine exited", zap.String("subdomain", t.Subdomain))
	}()

	// Build the WebSocket URL (ws:// or wss://)
	wsBase := strings.Replace(c.baseURL, "http://", "ws://", 1)
	wsBase = strings.Replace(wsBase, "https://", "wss://", 1)
	wsURL := fmt.Sprintf("%s/agent/ws?tunnel_id=%s&machine_id=%s&token=%s",
		wsBase,
		url.QueryEscape(t.TunnelID),
		url.QueryEscape(c.machineID),
		url.QueryEscape(t.AgentToken),
	)

	retryDelay := 3 * time.Second
	const maxDelay = 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := c.runConn(ctx, wsURL, t); err != nil {
			c.logger.Warn("Tunnel disconnected, will retry",
				zap.String("subdomain", t.Subdomain),
				zap.Error(err),
				zap.Duration("retry_in", retryDelay))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
			if retryDelay < maxDelay {
				retryDelay *= 2
			}
		}
	}
}

// runConn dials vsay-tunnel, exchanges messages, and returns when disconnected.
func (c *Client) runConn(ctx context.Context, wsURL string, t tunnelInfo) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	c.logger.Info("Tunnel connected",
		zap.String("subdomain", t.Subdomain),
		zap.String("public_url", t.PublicURL),
		zap.Int("local_port", t.LocalPort))

	// gorilla/websocket is NOT concurrent-safe for writes — use a serialiser channel
	sendCh := make(chan []byte, 256)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range sendCh {
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}()
	defer func() {
		close(sendCh)
		<-writerDone
	}()

	// Keepalive: send a WebSocket ping every 30s
	pingStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = conn.WriteMessage(websocket.PingMessage, nil) // best-effort keepalive; a dead conn surfaces via the read loop
			case <-pingStop:
				return
			}
		}
	}()
	defer close(pingStop)

	const readDeadline = 70 * time.Second
	_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readDeadline))
	})

	localBase := fmt.Sprintf("http://localhost:%d", t.LocalPort)

	// Reader loop — only "request" messages come from the server
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))

		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type != typeRequest {
			continue // ignore "connected" and other control messages
		}

		var req tunnelRequest
		if err := json.Unmarshal(env.Data, &req); err != nil {
			continue
		}

		// Handle each HTTP request concurrently — responses go back via sendCh
		go func(req tunnelRequest) {
			resp := c.forwardHTTP(localBase, req)
			respRaw, _ := json.Marshal(resp)
			data, _ := json.Marshal(envelope{Type: typeResponse, Data: respRaw})
			select {
			case sendCh <- data:
			default:
				c.logger.Warn("Send channel full, dropping response", zap.String("req_id", req.ID))
			}
		}(req)
	}
}

// forwardHTTP makes an HTTP request to the local application and returns the response.
func (c *Client) forwardHTTP(baseURL string, req tunnelRequest) tunnelResponse {
	// Decode optional body
	var bodyReader io.Reader
	if req.Body != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.Body)
		if err == nil && len(decoded) > 0 {
			bodyReader = strings.NewReader(string(decoded))
		}
	}

	fullURL := baseURL + req.URL
	httpReq, err := http.NewRequest(req.Method, fullURL, bodyReader)
	if err != nil {
		return errResponse(req.ID, 502, "failed to build request: "+err.Error())
	}

	// Copy headers from tunnel request to local request
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	// Remove hop-by-hop headers before forwarding
	for _, h := range hopByHop {
		httpReq.Header.Del(h)
	}
	// Keep-alive to local app is fine; don't override Host
	httpReq.Header.Del("Host")

	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return errResponse(req.ID, 502, "upstream error: "+err.Error())
	}
	defer httpResp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 10<<20)) // 10 MB cap

	// Flatten response headers, strip hop-by-hop
	headers := make(map[string]string, len(httpResp.Header))
	for k, vs := range httpResp.Header {
		headers[k] = strings.Join(vs, ", ")
	}
	for _, h := range hopByHop {
		delete(headers, h)
	}

	return tunnelResponse{
		ID:      req.ID,
		Status:  httpResp.StatusCode,
		Headers: headers,
		Body:    base64Encode(body),
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func errResponse(id string, status int, msg string) tunnelResponse {
	return tunnelResponse{
		ID:      id,
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain"},
		Body:    base64Encode([]byte(msg)),
	}
}

func base64Encode(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
