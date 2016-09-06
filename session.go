package smux

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
)

const (
	defaultAcceptBacklog = 1024
	defaultCloseWait     = 1024
)

const (
	errBrokenPipe      = "broken pipe"
	errInvalidProtocol = "invalid protocol version"
)

// Session defines a multiplexed connection for streams
type Session struct {
	conn      io.ReadWriteCloser
	writeLock sync.Mutex

	config       *Config
	nextStreamID uint32 // next stream identifier

	bucket     int32      // token bucket
	bucketCond *sync.Cond // used for waiting for tokens

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.Mutex         // locks streams

	die            chan struct{} // flag session has died
	dieLock        sync.Mutex
	chAccepts      chan *Stream
	chClosedStream chan uint32

	dataReady int32 // flag data has arrived
}

func newSession(config *Config, conn io.ReadWriteCloser, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.chClosedStream = make(chan uint32, defaultCloseWait)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketCond = sync.NewCond(&sync.Mutex{})
	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 2
	}
	go s.recvLoop()
	go s.monitor()
	go s.keepalive()
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, errors.New(errBrokenPipe)
	}

	sid := atomic.AddUint32(&s.nextStreamID, 2)
	stream := newStream(sid, s.config.MaxFrameSize, s)

	s.streamLock.Lock()
	s.streams[sid] = stream
	s.streamLock.Unlock()

	s.writeFrame(newFrame(cmdSYN, sid))
	return stream, nil
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-s.die:
		return nil, errors.New(errBrokenPipe)
	}
}

// Close is used to close the session and all streams.
func (s *Session) Close() error {
	s.dieLock.Lock()
	defer s.dieLock.Unlock()

	select {
	case <-s.die:
		return errors.New(errBrokenPipe)
	default:
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		s.conn.Close()
		close(s.die)
		s.bucketCond.Signal()
	}
	return nil
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	go func() {
		select {
		case s.chClosedStream <- sid:
		case <-s.die:
		}
	}()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.bucketCond.Signal()
	}
}

// session read a frame from underlying connection
// it's data is pointed to the input buffer
func (s *Session) readFrame(buffer []byte) (f Frame, err error) {
	if _, err := io.ReadFull(s.conn, buffer[:headerSize]); err != nil {
		return f, errors.Wrap(err, "readFrame")
	}

	dec := rawHeader(buffer)
	if dec.Version() != version {
		return f, errors.New(errInvalidProtocol)
	}

	if length := dec.Length(); length > 0 {
		if _, err := io.ReadFull(s.conn, buffer[headerSize:headerSize+length]); err != nil {
			return f, errors.Wrap(err, "readFrame")
		}
		f.ZeroCopyUnmarshal(buffer[:headerSize+length])
		return f, nil
	}
	f.ZeroCopyUnmarshal(buffer[:headerSize])
	return f, nil
}

// monitors streams
func (s *Session) monitor() {
	for {
		select {
		case sid := <-s.chClosedStream:
			s.streamLock.Lock()
			if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
				if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
					s.bucketCond.Signal()
				}
			}
			delete(s.streams, sid)
			s.streamLock.Unlock()
		case <-s.die:
			return
		}
	}
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	buffer := make([]byte, (1<<16)+headerSize)
	for {
		s.bucketCond.L.Lock()
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			s.bucketCond.Wait()
		}
		s.bucketCond.L.Unlock()

		if s.IsClosed() {
			return
		}

		if f, err := s.readFrame(buffer); err == nil {
			atomic.StoreInt32(&s.dataReady, 1)

			switch f.cmd {
			case cmdNOP:
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[f.sid]; !ok {
					stream := newStream(f.sid, s.config.MaxFrameSize, s)
					s.streams[f.sid] = stream
					go func() {
						select {
						case s.chAccepts <- stream:
						case <-s.die:
						}
					}()
				}
				s.streamLock.Unlock()
			case cmdRST:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					stream.markRST()
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			case cmdPSH:
				s.streamLock.Lock()
				if stream, ok := s.streams[f.sid]; ok {
					atomic.AddInt32(&s.bucket, -int32(len(f.data)))
					stream.pushBytes(f.data)
					stream.notifyReadEvent()
				}
				s.streamLock.Unlock()
			default:
				s.Close()
				return
			}
		} else {
			s.Close()
			return
		}
	}
}

func (s *Session) keepalive() {
	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerPing.Stop()
	defer tickerTimeout.Stop()
	for {
		select {
		case <-tickerPing.C:
			s.writeFrame(newFrame(cmdNOP, 0))
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				s.Close()
				return
			}
		case <-s.die:
			return
		}
	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	bts, _ := f.MarshalBinary()
	s.writeLock.Lock()
	n, err = s.conn.Write(bts)
	s.writeLock.Unlock()
	return n, err
}

// writeBinary writes the byte slice to the underlying connection
func (s *Session) writeBinary(bts []byte) (n int, err error) {
	s.writeLock.Lock()
	n, err = s.conn.Write(bts)
	s.writeLock.Unlock()
	return n, err
}
