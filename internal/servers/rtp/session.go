package rtp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/rtpreceiver"
	"github.com/bluenviron/gortsplib/v5/pkg/rtptime"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/bluenviron/mediamtx/internal/counterdumper"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

type rtpMedia struct {
	desc *description.Media
}

type rtpFormat struct {
	desc        format.Format
	rtpReceiver *rtpreceiver.Receiver
}

func (f *rtpFormat) initialize() {
	f.rtpReceiver = &rtpreceiver.Receiver{
		ClockRate:            f.desc.ClockRate(),
		UnrealiableTransport: true,
		Period:               10 * time.Second,
		WritePacketRTCP: func(_ rtcp.Packet) {
		},
	}
}

// Session represents a RTP session for a stream path.
type Session struct {
	streamPath string
	desc       *description.Session
	timeout    time.Duration
	server     *Server
	created    time.Time
	lastPacket time.Time

	stream               *stream.Stream
	timeDecoder          *rtptime.GlobalDecoder
	mediasByPayloadType  map[uint8]*rtpMedia
	formatsByPayloadType map[uint8]*rtpFormat
	packetsLost          *counterdumper.CounterDumper
	decodeErrors         *counterdumper.CounterDumper

	ctx       context.Context
	ctxCancel func()
	mutex     sync.RWMutex
}

func (s *Session) initialize() error {
	ctx, ctxCancel := context.WithCancel(context.Background())
	s.ctx = ctx
	s.ctxCancel = ctxCancel

	s.timeDecoder = &rtptime.GlobalDecoder{}
	s.timeDecoder.Initialize()

	s.mediasByPayloadType = make(map[uint8]*rtpMedia)
	s.formatsByPayloadType = make(map[uint8]*rtpFormat)

	for _, descMedia := range s.desc.Medias {
		rtpMedia := &rtpMedia{
			desc: descMedia,
		}

		for _, descFormat := range descMedia.Formats {
			rtpFormat := &rtpFormat{
				desc: descFormat,
			}
			rtpFormat.initialize()

			s.mediasByPayloadType[descFormat.PayloadType()] = rtpMedia
			s.formatsByPayloadType[descFormat.PayloadType()] = rtpFormat
		}
	}

	s.packetsLost = &counterdumper.CounterDumper{
		OnReport: func(val uint64) {
			s.Log(logger.Warn, "%d RTP %s lost",
				val,
				func() string {
					if val == 1 {
						return "packet"
					}
					return "packets"
				}())
		},
	}
	s.packetsLost.Start()

	s.decodeErrors = &counterdumper.CounterDumper{
		OnReport: func(val uint64) {
			s.Log(logger.Warn, "%d decode %s",
				val,
				func() string {
					if val == 1 {
						return "error"
					}
					return "errors"
				}())
		},
	}
	s.decodeErrors.Start()

	// Start timeout checker
	if s.timeout > 0 {
		go s.timeoutChecker()
	}

	return nil
}

func (s *Session) close() {
	s.ctxCancel()

	if s.packetsLost != nil {
		s.packetsLost.Stop()
	}
	if s.decodeErrors != nil {
		s.decodeErrors.Stop()
	}

	s.mutex.Lock()
	if s.stream != nil {
		// Remove publisher from path
		// The path will handle cleanup when it detects the publisher is closed
		s.stream = nil
	}
	s.mutex.Unlock()
}

func (s *Session) timeoutChecker() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mutex.RLock()
			lastPacket := s.lastPacket
			s.mutex.RUnlock()

			if time.Since(lastPacket) > s.timeout {
				s.Log(logger.Info, "session timeout, closing")
				s.server.Close()
				return
			}
		}
	}
}

func (s *Session) handlePacket(pkt *rtp.Packet) {
	s.mutex.Lock()
	s.lastPacket = time.Now()

	// Create stream if not exists
	if s.stream == nil {
		// Add publisher to path
		req := defs.PathAddPublisherReq{
			Author:             s,
			Desc:               s.desc,
			GenerateRTPPackets: true,
			FillNTP:            true,
			AccessRequest: defs.PathAccessRequest{
				Name: s.streamPath,
			},
			Res: make(chan defs.PathAddPublisherRes),
		}

		s.mutex.Unlock()

		_, stream, err := s.server.PathManager.AddPublisher(req)
		if err != nil {
			s.Log(logger.Error, "failed to add publisher: %v", err)
			return
		}

		s.mutex.Lock()
		s.stream = stream
		s.mutex.Unlock()

		s.Log(logger.Info, "stream created for path %s", s.streamPath)
	} else {
		s.mutex.Unlock()
	}

	s.mutex.RLock()
	stream := s.stream
	media, ok := s.mediasByPayloadType[pkt.PayloadType]
	forma := s.formatsByPayloadType[pkt.PayloadType]
	s.mutex.RUnlock()

	if !ok || forma == nil {
		return
	}

	pkts, lost := forma.rtpReceiver.ProcessPacket2(pkt, time.Now(), forma.desc.PTSEqualsDTS(pkt))

	if lost != 0 {
		s.packetsLost.Add(lost)
	}

	for _, pkt := range pkts {
		s.mutex.RLock()
		timeDecoder := s.timeDecoder
		descFormat := forma.desc
		descMedia := media.desc
		s.mutex.RUnlock()

		pts, ok2 := timeDecoder.Decode(descFormat, pkt)
		if !ok2 {
			continue
		}

		// SPS can be accessed by type assertion for H264 and H265 formats:
		// if h264Format, ok := descFormat.(*format.H264); ok {
		//     sps := h264Format.SPS
		// } else if h265Format, ok := descFormat.(*format.H265); ok {
		//     sps := h265Format.SPS
		// }

		stream.WriteRTPPacket(descMedia, descFormat, pkt, time.Time{}, pts)
	}
}

// Log implements logger.Writer.
func (s *Session) Log(level logger.Level, format string, args ...any) {
	s.server.Parent.Log(level, "[RTP session path=%s] "+format,
		append([]any{s.streamPath}, args...)...)
}

// GetStreamPath returns the stream path.
func (s *Session) GetStreamPath() string {
	return s.streamPath
}

// GetCreated returns when the session was created.
func (s *Session) GetCreated() time.Time {
	return s.created
}

// GetLastPacket returns when the last packet was received.
func (s *Session) GetLastPacket() time.Time {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.lastPacket
}

// Close implements defs.Publisher.
func (s *Session) Close() {
	s.close()
}

// APISourceDescribe implements defs.Source.
func (s *Session) APISourceDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "rtpSession",
		ID:   fmt.Sprintf("path:%s", s.streamPath),
	}
}
