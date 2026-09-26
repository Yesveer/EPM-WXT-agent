package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	agentv1 "github.com/vsay/vsay-agent/proto/agent/v1"
	commonv1 "github.com/vsay/vsay-agent/proto/common/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Client wraps gRPC client for agent communication
type Client struct {
	conn          *grpc.ClientConn
	agentClient   agentv1.AgentServiceClient
	logger        *zap.Logger
	agentID       string
	token         string
	stream        agentv1.AgentService_StreamClient
	streamContext context.Context
	streamCancel  context.CancelFunc
	streamErr     chan error // Signal stream errors
	// sendMu serializes stream.Send. gRPC client streams are NOT safe for concurrent
	// Send, but multiple goroutines call it (terminal output, heartbeats, and — under
	// heavy load — the remote-desktop port-forward relay). Without this lock those
	// concurrent sends corrupt the stream framing, which shows up as the desktop
	// tunnel dying under RDP's high throughput.
	sendMu sync.Mutex
}

// NewClient creates a new gRPC client
func NewClient(grpcURL, token string, useTLS bool, caCertFile string, logger *zap.Logger) (*Client, error) {
	var opts []grpc.DialOption

	if useTLS {
		tlsConfig := &tls.Config{}
		if caCertFile != "" {
			caPEM, err := os.ReadFile(caCertFile) // #nosec G304 -- caCertFile is agent config (CLI flag / config file), not request input
			if err != nil {
				return nil, fmt.Errorf("read CA cert: %w", err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(caPEM)
			tlsConfig.RootCAs = pool
			logger.Info("gRPC TLS using pinned CA cert", zap.String("ca", caCertFile))
		} else {
			logger.Info("gRPC TLS using system CA pool")
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = []grpc.DialOption{
			grpc.WithTransportCredentials(creds),
			grpc.WithBlock(),
			grpc.WithTimeout(10 * time.Second),
		}
		logger.Info("gRPC client configured with TLS")
	} else {
		opts = []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
			grpc.WithTimeout(10 * time.Second),
		}
		logger.Info("gRPC client running without TLS (insecure)")
	}

	conn, err := grpc.Dial(grpcURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}

	return &Client{
		conn:        conn,
		agentClient: agentv1.NewAgentServiceClient(conn),
		logger:      logger,
		token:       token,
	}, nil
}

// NewClientMTLS creates a gRPC client with mutual TLS.
//
// serverName is the expected server hostname (SERVER_DOMAIN). It is used for SNI
// and certificate verification. If empty, the hostname is derived from grpcURL.
// caFile pins the trust anchor — only certs signed by this CA are accepted.
// TLS 1.3 is enforced; lower versions are rejected.
func NewClientMTLS(grpcURL, token, certFile, keyFile, caFile, serverName string, logger *zap.Logger) (*Client, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mTLS: failed to load client cert/key: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13, // TLS 1.3 minimum — no downgrade attacks
	}

	// ServerName enforces that we only accept a cert for SERVER_DOMAIN.
	// Without this, any cert signed by the CA would be accepted for any hostname.
	if serverName != "" {
		tlsConfig.ServerName = serverName
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile) // #nosec G304 -- caFile is agent config (CLI flag / config file), not request input
		if err != nil {
			return nil, fmt.Errorf("mTLS: failed to read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("mTLS: failed to parse CA cert from %s", caFile)
		}
		tlsConfig.RootCAs = pool
		logger.Info("mTLS pinned CA cert", zap.String("ca", caFile), zap.String("server_name", serverName))
	} else {
		logger.Info("mTLS using system CA pool", zap.String("server_name", serverName))
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithBlock(),
		grpc.WithTimeout(10 * time.Second),
	}

	logger.Info("gRPC connecting with mTLS", zap.String("url", grpcURL))
	conn, err := grpc.Dial(grpcURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial (mTLS): %w", err)
	}

	return &Client{
		conn:        conn,
		agentClient: agentv1.NewAgentServiceClient(conn),
		logger:      logger,
		token:       token,
	}, nil
}

// NewClientMTLSCert is like NewClientMTLS but accepts an already-loaded tls.Certificate
// instead of cert/key file paths. Used when certs are decrypted in-memory from encrypted
// on-disk storage so the plaintext key never hits the filesystem after initial decrypt.
func NewClientMTLSCert(grpcURL, token string, cert tls.Certificate, caFile, serverName string, logger *zap.Logger) (*Client, error) {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	if serverName != "" {
		tlsConfig.ServerName = serverName
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile) // #nosec G304 -- caFile is agent config (CLI flag / config file), not request input
		if err != nil {
			return nil, fmt.Errorf("mTLS: failed to read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("mTLS: failed to parse CA cert from %s", caFile)
		}
		tlsConfig.RootCAs = pool
		logger.Info("mTLS pinned CA cert (encrypted keys)", zap.String("ca", caFile), zap.String("server_name", serverName))
	} else {
		logger.Info("mTLS using system CA pool (encrypted keys)", zap.String("server_name", serverName))
	}

	// TLS handshake probe — do a raw TLS dial BEFORE gRPC so we get the actual
	// TLS error (cert SAN mismatch, wrong CA, etc.) instead of the generic
	// "context deadline exceeded" that grpc.WithBlock() produces.
	tlsConn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp", grpcURL, tlsConfig,
	)
	if err != nil {
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}
	_ = tlsConn.Close() // probe connection only, used just to validate the handshake above

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithBlock(),
		grpc.WithTimeout(10 * time.Second),
	}

	logger.Info("gRPC connecting with mTLS (encrypted keys)", zap.String("url", grpcURL))
	conn, err := grpc.Dial(grpcURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial (mTLS): %w", err)
	}

	return &Client{
		conn:        conn,
		agentClient: agentv1.NewAgentServiceClient(conn),
		logger:      logger,
		token:       token,
	}, nil
}

// Close closes the gRPC connection
func (c *Client) Close() error {
	if c.streamCancel != nil {
		c.streamCancel()
	}
	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	return c.conn.Close()
}

// Register registers the agent with backend
func (c *Client) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	// Add token to metadata
	md := metadata.New(map[string]string{
		"authorization": "Bearer " + c.token,
	})
	ctx = metadata.NewOutgoingContext(ctx, md)

	resp, err := c.agentClient.Register(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("registration failed: %w", err)
	}

	c.agentID = resp.AgentId
	c.logger.Info("Agent registered",
		zap.String("agent_id", resp.AgentId),
		zap.Bool("approved", resp.Approved),
	)

	return resp, nil
}

// SendHeartbeat sends heartbeat to backend
func (c *Client) SendHeartbeat(ctx context.Context, stats *commonv1.ResourceStats) error {
	req := &agentv1.HeartbeatRequest{
		AgentId: c.agentID,
		Stats:   stats,
	}

	// Add token to metadata
	md := metadata.New(map[string]string{
		"authorization": "Bearer " + c.token,
	})
	ctx = metadata.NewOutgoingContext(ctx, md)

	_, err := c.agentClient.Heartbeat(ctx, req)
	if err != nil {
		return fmt.Errorf("heartbeat failed: %w", err)
	}

	return nil
}

// StartStream starts bidirectional streaming
func (c *Client) StartStream(ctx context.Context, messageHandler func(*agentv1.ServerMessage) error) error {
	// Add token to metadata
	md := metadata.New(map[string]string{
		"authorization": "Bearer " + c.token,
	})
	ctx = metadata.NewOutgoingContext(ctx, md)

	streamCtx, cancel := context.WithCancel(ctx)
	c.streamContext = streamCtx
	c.streamCancel = cancel
	c.streamErr = make(chan error, 1)

	stream, err := c.agentClient.Stream(streamCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create stream: %w", err)
	}

	c.stream = stream
	c.logger.Info("gRPC stream established")

	// Send first message to identify agent
	firstMsg := &agentv1.AgentMessage{
		AgentId: c.agentID,
		Payload: &agentv1.AgentMessage_Status{
			Status: &agentv1.StatusUpdate{
				Status:  "connected",
				Message: "Agent stream connected",
			},
		},
	}
	if err := stream.Send(firstMsg); err != nil {
		cancel()
		return fmt.Errorf("failed to send first message: %w", err)
	}

	// Start receiving messages
	go c.receiveMessages(messageHandler)

	// Wait for stream error or context cancellation
	select {
	case err := <-c.streamErr:
		c.logger.Info("Stream ended", zap.Error(err))
		return err
	case <-ctx.Done():
		c.logger.Info("Stream context canceled")
		return ctx.Err()
	}
}

// receiveMessages receives messages from stream
func (c *Client) receiveMessages(handler func(*agentv1.ServerMessage) error) {
	for {
		msg, err := c.stream.Recv()
		if err == io.EOF {
			c.logger.Info("Stream closed by server (EOF)")
			select {
			case c.streamErr <- io.EOF:
			default:
			}
			return
		}
		if err != nil {
			c.logger.Error("Stream error", zap.Error(err))
			select {
			case c.streamErr <- err:
			default:
			}
			return
		}

		if err := handler(msg); err != nil {
			c.logger.Error("Error handling message", zap.Error(err))
		}
	}
}

// SendMessage sends a message to backend
func (c *Client) SendMessage(msg *agentv1.AgentMessage) error {
	if c.stream == nil {
		return fmt.Errorf("stream not established")
	}

	msg.AgentId = c.agentID
	c.sendMu.Lock()
	err := c.stream.Send(msg)
	c.sendMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// SendHeartbeatMessage sends heartbeat via stream
func (c *Client) SendHeartbeatMessage(timestamp *commonv1.Timestamp, stats *commonv1.ResourceStats) error {
	msg := &agentv1.AgentMessage{
		AgentId: c.agentID,
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{
				Timestamp: timestamp,
				Stats:     stats,
			},
		},
	}

	return c.SendMessage(msg)
}

// SendCommandOutput sends command output to backend
func (c *Client) SendCommandOutput(commandID string, stdout, stderr []byte, exitCode int32, completed bool) error {
	msg := &agentv1.AgentMessage{
		AgentId: c.agentID,
		Payload: &agentv1.AgentMessage_CommandOutput{
			CommandOutput: &agentv1.CommandOutput{
				CommandId: commandID,
				Stdout:    stdout,
				Stderr:    stderr,
				ExitCode:  exitCode,
				Completed: completed,
			},
		},
	}

	return c.SendMessage(msg)
}

// SendTerminalOutput sends terminal output to backend
func (c *Client) SendTerminalOutput(sessionID string, data []byte) error {
	msg := &agentv1.AgentMessage{
		AgentId: c.agentID,
		Payload: &agentv1.AgentMessage_TerminalOutput{
			TerminalOutput: &agentv1.TerminalOutput{
				SessionId: sessionID,
				Data:      data,
			},
		},
	}

	return c.SendMessage(msg)
}

// SendStatusUpdate sends status update to backend
func (c *Client) SendStatusUpdate(status, message string) error {
	msg := &agentv1.AgentMessage{
		AgentId: c.agentID,
		Payload: &agentv1.AgentMessage_Status{
			Status: &agentv1.StatusUpdate{
				Status:  status,
				Message: message,
			},
		},
	}

	return c.SendMessage(msg)
}
