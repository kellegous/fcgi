package fcgi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

type recType uint8

const (
	typeBeginRequest recType = iota + 1
	typeAbortRequest
	typeEndRequest
	typeParams
	typeStdin
	typeStdout
	typeStderr
	typeData
	typeGetValues
	typeGetValuesResult
	typeUnknownType
)

type header struct {
	Version       uint8
	Type          recType
	ID            uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

const (
	maxWrite           = 65535
	maxPad             = 0
	fcgiVersion  uint8 = 1
	flagKeepConn uint8 = 1
)

const (
	roleResponder uint16 = iota + 1
	roleAuthorizer
	roleFilter
)

const (
	statusRequestComplete = iota
	statusCantMultiplex
	statusOverloaded
	statusUnknownRole
)

// Conn ...
type Conn struct {
	wl sync.Mutex
	cn net.Conn

	sl sync.RWMutex
	sm map[uint16]*Request
	id uint16
}

// Close ...
func (c *Conn) Close() error {
	c.shutdown(io.EOF)
	return c.cn.Close()
}

func (c *Conn) sub(r *Request) {
	c.sl.Lock()
	defer c.sl.Unlock()
	c.id++
	r.id = c.id
	c.sm[c.id] = r
}

func (c *Conn) unsub(id uint16) {
	c.sl.Lock()
	defer c.sl.Unlock()
	delete(c.sm, id)
}

func (c *Conn) sendBytes(p []byte) error {
	c.wl.Lock()
	defer c.wl.Unlock()
	_, err := c.cn.Write(p)
	return err
}

func (c *Conn) send(id uint16, recType recType, w *buffer) error {
	defer w.Reset()
	w.WriteHeader(id, recType, w.Len())
	return c.sendBytes(w.Bytes())
}

// ParamsFromRequest ...
func ParamsFromRequest(r *http.Request) map[string]string {
	params := map[string]string{
		"REQUEST_METHOD":  r.Method,
		"SERVER_PROTOCOL": fmt.Sprintf("HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor),
		"HTTP_HOST":       r.Host,
		"CONTENT_LENGTH":  fmt.Sprintf("%d", r.ContentLength),
		"CONTENT_TYPE":    r.Header.Get("Content-Type"),
		"REQUEST_URI":     r.RequestURI,
		"PATH_INFO":       r.URL.Path,
	}

	for k, v := range r.Header {
		name := fmt.Sprintf("HTTP_%s",
			strings.ToUpper(strings.Replace(k, "-", "_", -1)))
		// TODO(knorton): What the fuck do these shit servers do with multi-value
		// headers?
		params[name] = v[0]
	}

	https := "Off"
	if r.TLS != nil && r.TLS.HandshakeComplete {
		https = "On"
	}
	params["HTTPS"] = https

	// TODO(knorton): REMOTE_HOST and REMOTE_PORT

	return params
}

func writeBeginReq(c *Conn, w *buffer, id uint16) error {
	binary.Write(w, binary.BigEndian, roleResponder) // role
	binary.Write(w, binary.BigEndian, flagKeepConn)  // flags
	w.Write([]byte{0, 0, 0, 0, 0})                   // reserved
	return c.send(id, typeBeginRequest, w)
}

func writeAbortReq(c *Conn, w *buffer, id uint16) error {
	return c.send(id, typeAbortRequest, w)
}

func encodeLength(b []byte, n uint32) int {
	if n > 127 {
		n |= 1 << 31
		binary.BigEndian.PutUint32(b, n)
		return 4
	}
	b[0] = byte(n)
	return 1
}

func writeParams(c *Conn, w *buffer, id uint16, params map[string]string) error {
	var b [8]byte
	for k, v := range params {
		n := encodeLength(b[:], uint32(len(k)))
		n += encodeLength(b[n:], uint32(len(v)))
		t := n + len(k) + len(v)

		// this header will never fit and must be discarded
		if t > w.Cap() {
			continue
		}

		if t > w.Free() {
			if err := c.send(id, typeParams, w); err != nil {
				return err
			}
		}

		w.Write(b[:n])
		w.Write([]byte(k))
		w.Write([]byte(v))
	}

	if w.Len() > 0 {
		if err := c.send(id, typeParams, w); err != nil {
			return err
		}
	}

	// send the empty params message
	return c.send(id, typeParams, w)
}

func writeStdin(c *Conn, w *buffer, id uint16, r io.Reader) error {
	if r != nil {
		for {
			err := w.CopyFrom(r)
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}

			if err := c.send(id, typeStdin, w); err != nil {
				return err
			}
		}
	}

	return c.send(id, typeStdin, w)
}

type buffer struct {
	ix int
	dt [maxWrite + maxPad + 8]byte
}

func (b *buffer) WriteHeader(id uint16, recType recType, n int) {
	b.dt[0] = byte(fcgiVersion)
	b.dt[1] = byte(recType)
	binary.BigEndian.PutUint16(b.dt[2:4], id)
	binary.BigEndian.PutUint16(b.dt[4:6], uint16(n))
	b.dt[6] = 0
	b.dt[7] = 0
}

func (b *buffer) Write(p []byte) (int, error) {
	n := len(p)
	copy(b.dt[b.ix:], p)
	b.ix += n
	return n, nil
}

func (b *buffer) CopyFrom(r io.Reader) error {
	n, err := r.Read(b.dt[b.ix:])
	if err != nil {
		return err
	}
	b.ix += n
	return nil
}

func (b *buffer) Reset() {
	b.ix = 8
}

func (b *buffer) Bytes() []byte {
	return b.dt[:b.ix]
}

func (b *buffer) Cap() int {
	return len(b.dt) - 8
}

func (b *buffer) Len() int {
	return b.ix - 8
}

func (b *buffer) Free() int {
	return len(b.dt) - b.ix
}

type stdout []byte
type stderr []byte

// Request ...
type Request struct {
	id uint16
	c  *Conn
	ce chan error
	cw chan interface{}
}

// Abort ...
func (r *Request) Abort() error {
	var buf buffer
	return writeAbortReq(r.c, &buf, r.id)
}

// ID ...
func (r *Request) ID() uint16 {
	return r.id
}

// Wait ...
func (r *Request) Wait() error {
	return <-r.ce
}

func (r *Request) receive(wout, werr io.Writer) {
	for item := range r.cw {
		switch t := item.(type) {
		case stdout:
			if _, err := wout.Write([]byte(t)); err != nil {
				sendErr(r.ce, err)
				return
			}
		case stderr:
			if _, err := werr.Write([]byte(t)); err != nil {
				sendErr(r.ce, err)
				return
			}
		}
	}
}

// BeginRequest ...
func (c *Conn) BeginRequest(
	params map[string]string,
	body io.Reader,
	wout io.Writer,
	werr io.Writer) (*Request, error) {

	r := &Request{
		c:  c,
		ce: make(chan error),
		cw: make(chan interface{}),
	}

	var buf buffer
	buf.Reset()

	c.sub(r)

	if err := writeBeginReq(c, &buf, r.id); err != nil {
		return nil, err
	}

	if err := writeParams(c, &buf, r.id, params); err != nil {
		return nil, err
	}

	go r.receive(wout, werr)

	if err := writeStdin(c, &buf, r.id, body); err != nil {
		close(r.cw)
		return nil, err
	}

	return r, nil
}

func sendErr(ch chan error, err error) bool {
	select {
	case ch <- err:
		return true
	default:
		return false
	}
}

func (c *Conn) shutdown(err error) {
	c.sl.Lock()
	defer c.sl.Unlock()
	for _, r := range c.sm {
		sendErr(r.ce, err)
	}
}

func (c *Conn) getReq(id uint16) *Request {
	c.sl.RLock()
	defer c.sl.RUnlock()
	return c.sm[id]
}

func receive(c *Conn) {
	var h header
	for {
		if err := binary.Read(c.cn, binary.BigEndian, &h); err != nil {
			c.shutdown(err)
			return
		}

		if h.Version != fcgiVersion {
			c.shutdown(errors.New("cgi: invalid fcgi version"))
			return
		}

		// TODO(knorton): These could be taken from a buffer pool
		buf := make([]byte, int(h.ContentLength)+int(h.PaddingLength))

		if _, err := io.ReadFull(c.cn, buf); err != nil {
			c.shutdown(err)
			return
		}

		buf = buf[:h.ContentLength]

		r := c.getReq(h.ID)
		if r == nil {
			continue
		}

		switch h.Type {
		case typeStdout:
			r.cw <- stdout(buf)
		case typeStderr:
			r.cw <- stderr(buf)
		case typeEndRequest:
			c.unsub(h.ID)
			r.ce <- nil
			return
		}
	}
}

// Dial ...
func Dial(network, addr string) (*Conn, error) {
	cn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}

	c := &Conn{
		cn: cn,
		sm: map[uint16]*Request{},
	}

	go receive(c)

	return c, nil
}
