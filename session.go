package yamux

import (
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

// Session is used to wrap a reliable ordered connection and to
// multiplex it into multiple streams.
type Session struct {
	// client is true if we are a client size connection
	client bool

	// config holds our configuration
	config *Config

	// conn is the underlying connection
	conn io.ReadWriteCloser

	// pings is used to track inflight pings
	pings    map[uint32]chan struct{}
	pingID   uint32
	pingLock sync.Mutex

	// remoteGoAway indicates the remote side does
	// not want futher connections
	remoteGoAway bool

	// localGoAway indicates that we should stop
	// accepting futher connections
	localGoAway bool

	// nextStreamID is the next stream we should
	// send. This depends if we are a client/server.
	nextStreamID uint32

	// streams maps a stream id to a stream
	streams    map[uint32]*Stream
	streamLock sync.RWMutex

	// acceptCh is used to pass ready streams to the client
	acceptCh chan *Stream

	// sendCh is used to mark a stream as ready to send,
	// or to send a header out directly.
	sendCh chan sendReady

	// shutdown is used to safely close a session
	shutdown     bool
	shutdownErr  error
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

// sendReady is used to either mark a stream as ready
// or to directly send a header
type sendReady struct {
	Hdr  []byte
	Body io.Reader
	Err  chan error
}

// newSession is used to construct a new session
func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := &Session{
		client:     client,
		config:     config,
		conn:       conn,
		pings:      make(map[uint32]chan struct{}),
		streams:    make(map[uint32]*Stream),
		acceptCh:   make(chan *Stream, config.AcceptBacklog),
		sendCh:     make(chan sendReady, 64),
		shutdownCh: make(chan struct{}),
	}
	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recv()
	go s.send()
	if config.EnableKeepAlive {
		go s.keepalive()
	}
	return s
}

// isShutdown does a safe check to see if we have shutdown
func (s *Session) isShutdown() bool {
	select {
	case <-s.shutdownCh:
		return true
	default:
		return false
	}
}

// Open is used to create a new stream
func (s *Session) Open() (*Stream, error) {
	if s.isShutdown() {
		return nil, ErrSessionShutdown
	}
	if s.remoteGoAway {
		return nil, ErrRemoteGoAway
	}

	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	// Check if we've exhaused the streams
	id := s.nextStreamID
	if id >= math.MaxUint32-1 {
		return nil, ErrStreamsExhausted
	}
	s.nextStreamID += 2

	// Register the stream
	stream := newStream(s, id, streamInit)
	s.streams[id] = stream

	// Send the window update to create
	return stream, stream.sendWindowUpdate()
}

// Accept is used to block until the next available stream
// is ready to be accepted.
func (s *Session) Accept() (net.Conn, error) {
	return s.AcceptStream()
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case stream := <-s.acceptCh:
		return stream, nil
	case <-s.shutdownCh:
		return nil, s.shutdownErr
	}
}

// Close is used to close the session and all streams.
// Attempts to send a GoAway before closing the connection.
func (s *Session) Close() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	if s.shutdown {
		return nil
	}
	s.shutdown = true
	if s.shutdownErr == nil {
		s.shutdownErr = ErrSessionShutdown
	}
	close(s.shutdownCh)
	s.conn.Close()

	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	for _, stream := range s.streams {
		stream.forceClose()
	}
	return nil
}

// GoAway can be used to prevent accepting further
// connections. It does not close the underlying conn.
func (s *Session) GoAway() error {
	s.localGoAway = true
	s.goAway(goAwayNormal)
	return nil
}

// Ping is used to measure the RTT response time
func (s *Session) Ping() (time.Duration, error) {
	// Get a channel for the ping
	ch := make(chan struct{})

	// Get a new ping id, mark as pending
	s.pingLock.Lock()
	id := s.pingID
	s.pingID++
	s.pings[id] = ch
	s.pingLock.Unlock()

	// Send the ping request
	hdr := header(make([]byte, headerSize))
	hdr.encode(typePing, flagSYN, 0, id)
	if err := s.waitForSend(hdr, nil); err != nil {
		return 0, err
	}

	// Wait for a response
	start := time.Now()
	select {
	case <-ch:
	case <-s.shutdownCh:
		return 0, ErrSessionShutdown
	}

	// Compute the RTT
	return time.Now().Sub(start), nil
}

// keepalive is a long running goroutine that periodically does
// a ping to keep the connection alive.
func (s *Session) keepalive() {
	for {
		select {
		case <-time.After(s.config.KeepAliveInterval):
			s.Ping()
		case <-s.shutdownCh:
			return
		}
	}
}

// waitForSend waits to send a header, checking for a potential shutdown
func (s *Session) waitForSend(hdr header, body io.Reader) error {
	errCh := make(chan error, 1)
	ready := sendReady{Hdr: hdr, Body: body, Err: errCh}
	select {
	case s.sendCh <- ready:
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
	select {
	case err := <-errCh:
		return err
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
}

// sendNoWait does a send without waiting
func (s *Session) sendNoWait(hdr header) error {
	select {
	case s.sendCh <- sendReady{Hdr: hdr}:
		return nil
	case <-s.shutdownCh:
		return ErrSessionShutdown
	}
}

// send is a long running goroutine that sends data
func (s *Session) send() {
	for {
		select {
		case ready := <-s.sendCh:
			// Send a header if ready
			if ready.Hdr != nil {
				sent := 0
				for sent < len(ready.Hdr) {
					n, err := s.conn.Write(ready.Hdr[sent:])
					if err != nil {
						s.exitErr(err)
						asyncSendErr(ready.Err, err)
						return
					}
					sent += n
				}
			}

			// Send data from a body if given
			if ready.Body != nil {
				_, err := io.Copy(s.conn, ready.Body)
				if err != nil {
					s.exitErr(err)
					asyncSendErr(ready.Err, err)
					return
				}
			}

			// No error, successful send
			asyncSendErr(ready.Err, nil)
		case <-s.shutdownCh:
			return
		}
	}
}

// recv is a long running goroutine that accepts new data
func (s *Session) recv() {
	hdr := header(make([]byte, headerSize))
	for !s.isShutdown() {
		// Read the header
		if _, err := io.ReadFull(s.conn, hdr); err != nil {
			s.exitErr(err)
			return
		}

		// Verify the version
		if hdr.Version() != protoVersion {
			s.exitErr(ErrInvalidVersion)
			return
		}

		// Switch on the type
		msgType := hdr.MsgType()
		switch msgType {
		case typeData:
			fallthrough
		case typeWindowUpdate:
			if err := s.handleStreamMessage(hdr); err != nil {
				s.exitErr(err)
				return
			}
		case typeGoAway:
			if err := s.handleGoAway(hdr); err != nil {
				s.exitErr(err)
				return
			}
		case typePing:
			if err := s.handlePing(hdr); err != nil {
				s.exitErr(err)
				return
			}
		default:
			s.exitErr(ErrInvalidMsgType)
			return
		}
	}
}

// handleStreamMessage handles either a data or window update frame
func (s *Session) handleStreamMessage(hdr header) error {
	// Check for a new stream creation
	id := hdr.StreamID()
	flags := hdr.Flags()
	if flags&flagSYN == flagSYN {
		if err := s.incomingStream(id); err != nil {
			return err
		}
	}

	// Get the stream
	s.streamLock.RLock()
	stream := s.streams[id]
	s.streamLock.RUnlock()

	// Make sure we have a stream
	if stream == nil {
		s.goAway(goAwayProtoErr)
		return ErrMissingStream
	}

	// Check if this is a window update
	if hdr.MsgType() == typeWindowUpdate {
		if err := stream.incrSendWindow(hdr, flags); err != nil {
			s.goAway(goAwayProtoErr)
			return err
		}
	}

	// Read the new data
	if err := stream.readData(hdr, flags, s.conn); err != nil {
		s.goAway(goAwayProtoErr)
		return err
	}
	return nil
}

// handlePing is invokde for a typePing frame
func (s *Session) handlePing(hdr header) error {
	flags := hdr.Flags()
	pingID := hdr.Length()

	// Check if this is a query, respond back
	if flags&flagSYN == flagSYN {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typePing, flagACK, 0, pingID)
		s.sendNoWait(hdr)
		return nil
	}

	// Handle a response
	s.pingLock.Lock()
	ch := s.pings[pingID]
	if ch != nil {
		delete(s.pings, pingID)
		close(ch)
	}
	s.pingLock.Unlock()
	return nil
}

// handleGoAway is invokde for a typeGoAway frame
func (s *Session) handleGoAway(hdr header) error {
	code := hdr.Length()
	switch code {
	case goAwayNormal:
		s.remoteGoAway = true
	case goAwayProtoErr:
		return fmt.Errorf("yamux protocol error")
	case goAwayInternalErr:
		return fmt.Errorf("remote yamux internal error")
	default:
		return fmt.Errorf("unexpected go away received")
	}
	return nil
}

// exitErr is used to handle an error that is causing
// the listener to exit.
func (s *Session) exitErr(err error) {
	s.shutdownErr = err
	s.Close()
}

// goAway is used to send a goAway message
func (s *Session) goAway(reason uint32) {
	hdr := header(make([]byte, headerSize))
	hdr.encode(typeGoAway, 0, 0, reason)
	s.sendNoWait(hdr)
}

// incomingStream is used to create a new incoming stream
func (s *Session) incomingStream(id uint32) error {
	// Reject immediately if we are doing a go away
	if s.localGoAway {
		hdr := header(make([]byte, headerSize))
		hdr.encode(typeWindowUpdate, flagRST, id, 0)
		s.sendNoWait(hdr)
		return nil
	}

	s.streamLock.Lock()
	defer s.streamLock.Unlock()

	// Check if stream already exists
	if _, ok := s.streams[id]; ok {
		s.goAway(goAwayProtoErr)
		s.exitErr(ErrDuplicateStream)
		return nil
	}

	// Register the stream
	stream := newStream(s, id, streamSYNReceived)
	s.streams[id] = stream

	// Check if we've exceeded the backlog
	select {
	case s.acceptCh <- stream:
		return nil
	default:
		// Backlog exceeded! RST the stream
		delete(s.streams, id)
		stream.sendHdr.encode(typeWindowUpdate, flagRST, id, 0)
		s.sendNoWait(stream.sendHdr)
	}
	return nil
}

// closeStream is used to close a stream once both sides have
// issued a close.
func (s *Session) closeStream(id uint32, withLock bool) {
	if !withLock {
		s.streamLock.Lock()
		defer s.streamLock.Unlock()
	}
	delete(s.streams, id)
}