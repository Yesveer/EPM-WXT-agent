package portforward

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"go.uber.org/zap"
)

type Manager struct {
	logger      *zap.Logger
	connections map[string]net.Conn
	mu          sync.RWMutex
	sendFunc    func(sessionID string, data []byte) error
}

type PortMessage struct {
	Type         string `json:"type"`
	ConnectionID string `json:"connection_id"`
	RemotePort   int    `json:"remote_port"`
	Data         string `json:"data,omitempty"`
	Error        string `json:"error,omitempty"`
}

func NewManager(logger *zap.Logger, sendFunc func(sessionID string, data []byte) error) *Manager {
	return &Manager{
		logger:      logger,
		connections: make(map[string]net.Conn),
		sendFunc:    sendFunc,
	}
}

func (m *Manager) HandleMessage(sessionID string, msg map[string]interface{}) {
	msgType, _ := msg["type"].(string)
	connectionID, _ := msg["connection_id"].(string)
	remotePort, _ := msg["remote_port"].(float64)

	switch msgType {
	case "port_connect":
		m.handleConnect(sessionID, connectionID, int(remotePort))
	case "port_data":
		data, _ := msg["data"].(string)
		m.handleData(connectionID, data)
	case "port_close":
		m.handleClose(connectionID)
	}
}

func (m *Manager) handleConnect(sessionID string, connectionID string, remotePort int) {
	m.logger.Info("Port forward connect request",
		zap.String("session_id", sessionID),
		zap.String("connection_id", connectionID),
		zap.Int("remote_port", remotePort))

	// Connect to the local port
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err != nil {
		m.logger.Error("Failed to connect to local port (is the RDP/VNC server running on it?)",
			zap.Int("port", remotePort),
			zap.Error(err))
		m.sendError(sessionID, connectionID, fmt.Sprintf("Failed to connect to port %d: %v", remotePort, err))
		return
	}

	m.mu.Lock()
	m.connections[connectionID] = conn
	m.mu.Unlock()

	m.logger.Info("Port forward connection established",
		zap.String("connection_id", connectionID),
		zap.Int("remote_port", remotePort))

	// Start reading from the connection and forwarding data back
	go m.readFromConnection(sessionID, connectionID, conn)
}

func (m *Manager) handleData(connectionID string, dataBase64 string) {
	m.mu.RLock()
	conn, exists := m.connections[connectionID]
	m.mu.RUnlock()

	if !exists {
		m.logger.Warn("Connection not found for data", zap.String("connection_id", connectionID))
		return
	}

	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		m.logger.Error("Failed to decode data", zap.Error(err))
		return
	}

	_, err = conn.Write(data)
	if err != nil {
		m.logger.Error("Failed to write to connection", zap.Error(err))
		m.handleClose(connectionID)
	}
}

func (m *Manager) handleClose(connectionID string) {
	m.mu.Lock()
	conn, exists := m.connections[connectionID]
	if exists {
		if err := conn.Close(); err != nil {
			m.logger.Warn("Failed to close port forward connection", zap.String("connection_id", connectionID), zap.Error(err))
		}
		delete(m.connections, connectionID)
	}
	m.mu.Unlock()

	m.logger.Info("Port forward connection closed", zap.String("connection_id", connectionID))
}

func (m *Manager) readFromConnection(sessionID string, connectionID string, conn net.Conn) {
	buffer := make([]byte, 32*1024) // 32KB buffer

	for {
		n, err := conn.Read(buffer)
		if err != nil {
			m.logger.Debug("Connection read ended",
				zap.String("connection_id", connectionID),
				zap.Error(err))
			m.sendClosed(sessionID, connectionID)
			m.handleClose(connectionID)
			return
		}

		if n > 0 {
			// Send data back to extension
			dataBase64 := base64.StdEncoding.EncodeToString(buffer[:n])
			m.sendData(sessionID, connectionID, dataBase64)
		}
	}
}

func (m *Manager) sendData(sessionID string, connectionID string, data string) {
	msg := PortMessage{
		Type:         "port_data",
		ConnectionID: connectionID,
		Data:         data,
	}
	m.sendMessage(sessionID, msg)
}

func (m *Manager) sendClosed(sessionID string, connectionID string) {
	msg := PortMessage{
		Type:         "port_closed",
		ConnectionID: connectionID,
	}
	m.sendMessage(sessionID, msg)
}

func (m *Manager) sendError(sessionID string, connectionID string, errMsg string) {
	msg := PortMessage{
		Type:         "port_error",
		ConnectionID: connectionID,
		Error:        errMsg,
	}
	m.sendMessage(sessionID, msg)
}

func (m *Manager) sendMessage(sessionID string, msg PortMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		m.logger.Error("Failed to marshal port message", zap.Error(err))
		return
	}
	if err := m.sendFunc(sessionID, data); err != nil {
		m.logger.Error("Failed to send port message", zap.Error(err))
	}
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, conn := range m.connections {
		if err := conn.Close(); err != nil {
			m.logger.Warn("Failed to close port forward connection", zap.String("connection_id", id), zap.Error(err))
		}
		delete(m.connections, id)
	}
}

// ActiveCount returns the number of open forwarded connections (RDP/VNC tunnels, etc.).
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}
