// Package rtp contains a RTP server for receiving RTP streams on a single port
// and routing them to a specific stream path.
package rtp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/sdp"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/restrictnetwork"
	"github.com/bluenviron/mediamtx/internal/stream"
	"github.com/pion/rtp"
)

// ErrServerNotFound is returned when a RTP server is not found.
var ErrServerNotFound = errors.New("RTP server not found")

type serverPathManager interface {
	FindPathConf(req defs.PathFindPathConfReq) (*conf.Path, error)
	AddPublisher(req defs.PathAddPublisherReq) (defs.Path, *stream.Stream, error)
}

type serverParent interface {
	logger.Writer
}

// Server is a RTP server that listens on a UDP port and routes all RTP packets
// to a specific stream path.
type Server struct {
	Port              int
	StreamPath        string
	SDP               string
	Timeout           time.Duration
	UDPReadBufferSize uint
	ReadTimeout       conf.Duration
	PathManager       serverPathManager
	Parent            serverParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup

	conn    net.PacketConn
	session *Session
	mutex   sync.RWMutex
}

// Initialize initializes the RTP server.
func (s *Server) Initialize() error {
	ctx, ctxCancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.ctxCancel = ctxCancel

	// Parse SDP and create session
	var sd sdp.SessionDescription
	err := sd.Unmarshal([]byte(s.SDP))
	if err != nil {
		return fmt.Errorf("invalid SDP: %w", err)
	}

	var desc description.Session
	err = desc.Unmarshal(&sd)
	if err != nil {
		return fmt.Errorf("failed to unmarshal description: %w", err)
	}

	s.mutex.Lock()
	s.session = &Session{
		streamPath: s.StreamPath,
		desc:       &desc,
		timeout:    s.Timeout,
		server:     s,
		created:    time.Now(),
		lastPacket: time.Now(),
	}
	s.mutex.Unlock()
	err = s.session.initialize()
	if err != nil {
		return fmt.Errorf("failed to initialize session: %w", err)
	}

	addr := fmt.Sprintf(":%d", s.Port)
	pc, err := net.ListenPacket(restrictnetwork.Restrict("udp", addr))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", s.Port, err)
	}

	if s.UDPReadBufferSize > 0 {
		if udpConn, ok := pc.(*net.UDPConn); ok {
			udpConn.SetReadBuffer(int(s.UDPReadBufferSize))
		}
	}

	s.conn = pc

	s.Log(logger.Info, "listening on port %d for stream path %s", s.Port, s.StreamPath)

	s.wg.Add(1)
	go s.run()

	return nil
}

// Close closes the RTP server.
func (s *Server) Close() {
	s.Log(logger.Info, "closing RTP server on port %d", s.Port)
	s.ctxCancel()
	if s.conn != nil {
		s.conn.Close()
	}
	s.wg.Wait()

	s.mutex.Lock()
	if s.session != nil {
		s.session.close()
		s.session = nil
	}
	s.mutex.Unlock()
}

// Log implements logger.Writer.
func (s *Server) Log(level logger.Level, format string, args ...any) {
	s.Parent.Log(level, "[RTP server port %d] "+format, append([]any{s.Port}, args...)...)
}

func (s *Server) run() {
	defer s.wg.Done()

	buf := make([]byte, 1500)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			if s.ctx.Err() == nil {
				s.Log(logger.Error, "read error: %v", err)
			}
			return
		}

		var pkt rtp.Packet
		err = pkt.Unmarshal(buf[:n])
		if err != nil {
			s.Log(logger.Debug, "failed to unmarshal RTP packet from %s: %v", addr, err)
			continue
		}

		s.handlePacket(&pkt, addr)
	}
}

func (s *Server) handlePacket(pkt *rtp.Packet, addr net.Addr) {
	s.mutex.RLock()
	session := s.session
	s.mutex.RUnlock()

	if session == nil {
		s.Log(logger.Debug, "received RTP packet but session not initialized, ignoring")
		return
	}

	session.handlePacket(pkt)
}

// GetSession returns the session.
func (s *Server) GetSession() *Session {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.session
}
