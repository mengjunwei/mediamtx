package rtp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
)

type managerParent interface {
	logger.Writer
}

// Manager manages multiple RTP servers.
type Manager struct {
	UDPReadBufferSize uint
	ReadTimeout       conf.Duration
	PathManager       serverPathManager
	Parent            managerParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup

	servers map[int]*Server // key: port
	mutex   sync.RWMutex
}

// Initialize initializes the RTP manager.
func (m *Manager) Initialize() error {
	ctx, ctxCancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.ctxCancel = ctxCancel
	m.servers = make(map[int]*Server)

	m.Log(logger.Debug, "RTP manager created")
	return nil
}

// Close closes the RTP manager.
func (m *Manager) Close() {
	m.Log(logger.Debug, "RTP manager is shutting down")
	m.ctxCancel()
	m.wg.Wait()

	m.mutex.Lock()
	for _, server := range m.servers {
		server.Close()
	}
	m.servers = nil
	m.mutex.Unlock()
}

// Log implements logger.Writer.
func (m *Manager) Log(level logger.Level, format string, args ...any) {
	m.Parent.Log(level, "[RTP manager] "+format, args...)
}

// GetServer returns a RTP server by port.
func (m *Manager) GetServer(port int) (*Server, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	server, ok := m.servers[port]
	if !ok {
		return nil, ErrServerNotFound
	}

	return server, nil
}

// OpenServer implements defs.APIRTPServer.
func (m *Manager) OpenServer(port int, streamPath string, sdp string, timeout time.Duration) (int, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if port == 0 {
		// Find an available port
		addr, err := net.ResolveUDPAddr("udp", ":0")
		if err != nil {
			return 0, fmt.Errorf("failed to resolve UDP address: %w", err)
		}

		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return 0, fmt.Errorf("failed to find available port: %w", err)
		}

		port = conn.LocalAddr().(*net.UDPAddr).Port
		conn.Close()
	}

	if _, exists := m.servers[port]; exists {
		return 0, fmt.Errorf("RTP server on port %d already exists", port)
	}

	server := &Server{
		Port:              port,
		StreamPath:        streamPath,
		SDP:               sdp,
		Timeout:           timeout,
		UDPReadBufferSize: m.UDPReadBufferSize,
		ReadTimeout:       m.ReadTimeout,
		PathManager:       m.PathManager,
		Parent:            m,
	}

	err := server.Initialize()
	if err != nil {
		return 0, err
	}

	m.servers[port] = server

	m.Log(logger.Info, "opened RTP server on port %d for stream path %s", port, streamPath)
	return server.Port, nil
}

// CloseServer implements defs.APIRTPServer.
func (m *Manager) CloseServer(port int) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	server, ok := m.servers[port]
	if !ok {
		return ErrServerNotFound
	}

	server.Close()
	delete(m.servers, port)

	m.Log(logger.Info, "closed RTP server on port %d", port)
	return nil
}

// ListServers implements defs.APIRTPServer.
func (m *Manager) ListServers() ([]*defs.APIRTPServerInfo, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	infos := make([]*defs.APIRTPServerInfo, 0, len(m.servers))
	for _, server := range m.servers {
		session := server.GetSession()
		streamPath := ""
		if session != nil {
			streamPath = session.GetStreamPath()
		}
		infos = append(infos, &defs.APIRTPServerInfo{
			Port:       server.Port,
			StreamPath: streamPath,
			Created:    time.Now(), // TODO: track creation time
		})
	}

	return infos, nil
}
