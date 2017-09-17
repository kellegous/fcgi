package fcgi

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
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

// Options ...
type Options struct {
	dialTimeout time.Duration
	timeout     time.Duration
}

type conn struct {
	net.Conn
	timeout time.Duration
}

// Client provides a way to dispatch FCGI requests to a specified server.
type Client struct {
	c  *conn
	sl sync.RWMutex
	sm map[uint16]*Request
	id uint16
}

// DialOption is a function passed to dial for configuring the client.
type DialOption func(*Options) error

func (c *conn) Write(b []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}

	return c.Conn.Write(b)
}

// Close ...
func (c *Client) Close() error {
	return c.shutdown(errors.New("client terminated"))
}

func (c *Client) sub(r *Request) {
	c.sl.Lock()
	defer c.sl.Unlock()
	c.id++
	r.id = c.id
	c.sm[c.id] = r
}

func (c *Client) unsub(id uint16) {
	c.sl.Lock()
	defer c.sl.Unlock()
	delete(c.sm, id)
}

// ParamsFromRequest provides a standard way to convert an http.Request to
// the key-value pairs that are passed as FCGI parameters.
func ParamsFromRequest(r *http.Request) map[string][]string {
	params := map[string][]string{
		"REQUEST_METHOD":  {r.Method},
		"SERVER_PROTOCOL": {fmt.Sprintf("HTTP/%d.%d", r.ProtoMajor, r.ProtoMinor)},
		"HTTP_HOST":       {r.Host},
		"CONTENT_LENGTH":  {fmt.Sprintf("%d", r.ContentLength)},
		"CONTENT_TYPE":    {r.Header.Get("Content-Type")},
		"REQUEST_URI":     {r.RequestURI},
		"PATH_INFO":       {r.URL.Path},
	}

	for key, vals := range r.Header {
		name := fmt.Sprintf("HTTP_%s",
			strings.ToUpper(strings.Replace(key, "-", "_", -1)))
		params[name] = vals
	}

	https := "Off"
	if r.TLS != nil && r.TLS.HandshakeComplete {
		https = "On"
	}
	params["HTTPS"] = []string{https}

	// TODO(knorton): REMOTE_HOST and REMOTE_PORT

	return params
}

// Write the beginning of a request into the given connection.
func writeBeginReq(c net.Conn, w *buffer, id uint16) error {
	binary.Write(w, binary.BigEndian, roleResponder) // role
	binary.Write(w, binary.BigEndian, flagKeepConn)  // flags
	w.Write([]byte{0, 0, 0, 0, 0})                   // reserved
	return w.WriteRecord(c, id, typeBeginRequest)
}

// Write an abort request into the given connection.
func writeAbortReq(c net.Conn, w *buffer, id uint16) error {
	return w.WriteRecord(c, id, typeAbortRequest)
}

// Encode the length of a key or value using FCGIs compressed length
// scheme. The encoded length is placed in b and the number of bytes
// that were required to encode the length is returned.
func encodeLength(b []byte, n uint32) int {
	if n > 127 {
		n |= 1 << 31
		binary.BigEndian.PutUint32(b, n)
		return 4
	}
	b[0] = byte(n)
	return 1
}

// Encode and write the given parameters into the connection. Note that the headers
// may be fragmented into several writes if they will not fit into a single write.
func writeParams(c net.Conn, w *buffer, id uint16, params map[string][]string) error {
	var b [8]byte
	for key, vals := range params {
		for _, val := range vals {
			// encode the key's length
			n := encodeLength(b[:], uint32(len(key)))

			// encode the value's length
			n += encodeLength(b[n:], uint32(len(val)))

			// the total lenth of this param
			t := n + len(key) + len(val)

			// this header itself is so big, it cannot fit into a
			// write so we just discard it.
			if t > w.Cap() {
				continue
			}

			// if this param would overflow the current buffer, go ahead
			// and send it.
			if t > w.Free() {
				if err := w.WriteRecord(c, id, typeParams); err != nil {
					return err
				}
			}

			w.Write(b[:n])
			w.Write([]byte(key))
			w.Write([]byte(val))
		}
	}

	if w.Len() > 0 {
		if err := w.WriteRecord(c, id, typeParams); err != nil {
			return err
		}
	}

	// send the empty params message
	return w.WriteRecord(c, id, typeParams)
}

// Copy the data from the given reader into the connection as stdin. Note that
// this may fragment the data into multiple writes.
func writeStdin(c net.Conn, w *buffer, id uint16, r io.Reader) error {
	if r != nil {
		for {
			err := w.CopyFrom(r)
			if err == io.EOF {
				break
			} else if err != nil {
				return err
			}

			if err := w.WriteRecord(c, id, typeStdin); err != nil {
				return err
			}
		}
	}

	return w.WriteRecord(c, id, typeStdin)
}

func statusFromHeaders(h http.Header) (int, error) {
	text := h.Get("Status")
	if text == "" {
		return http.StatusOK, nil
	}

	ix := strings.Index(text, " ")
	if ix >= 0 {
		text = text[:ix]
	}

	s, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, err
	}

	return int(s), nil
}

func filterHeaders(h http.Header) {
	h.Del("Status")
}

func (c *Client) ServeHTTP(
	params map[string][]string,
	w http.ResponseWriter,
	r *http.Request) {

	pr, pw := io.Pipe()

	req, err := c.BeginRequest(
		params,
		r.Body,
		pw,
		os.Stderr)
	if err != nil {
		log.Panic(err)
	}

	go func() {
		if err := req.Wait(); err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	br := bufio.NewReader(pr)
	tr := textproto.NewReader(br)
	mh, err := tr.ReadMIMEHeader()
	if err != nil {
		log.Panic(err)
	}

	h := http.Header(mh)
	s, err := statusFromHeaders(h)
	if err != nil {
		log.Panic(err)
	}

	w.WriteHeader(s)
	for key, vals := range h {
		for _, val := range vals {
			w.Header().Add(key, val)
		}
	}

	if _, err := io.Copy(w, br); err != nil {
		log.Panic(err)
	}
}

// BeginRequest ...
func (c *Client) BeginRequest(
	params map[string][]string,
	body io.Reader,
	wout io.Writer,
	werr io.Writer) (*Request, error) {

	r := &Request{
		c:    c,
		cw:   make(chan interface{}),
		done: make(chan struct{}, 1),
		out:  wout,
		err:  werr,
	}

	var buf buffer
	buf.Reset()

	c.sub(r)

	if err := writeBeginReq(c.c, &buf, r.id); err != nil {
		return nil, err
	}

	if err := writeParams(c.c, &buf, r.id, params); err != nil {
		return nil, err
	}

	// backends may start emitting output before all stdin has been transmitted,
	// so we have to be processing messages from the backend before stdin can
	// be fully accepted.
	go func() {
		if err := writeStdin(c.c, &buf, r.id, body); err != nil {
			// send the error only if the request has not already been
			// canceled.
			select {
			case r.cw <- err:
			case <-r.done:
			}
		}
	}()

	return r, nil
}

func (c *Client) shutdown(err error) error {
	// dispatch errors to all active requests
	for id, r := range c.sm {
		c.unsub(id)
		r.cw <- err
	}

	return c.c.Close()
}

func (c *Client) getReq(id uint16) *Request {
	c.sl.RLock()
	defer c.sl.RUnlock()
	return c.sm[id]
}

func receive(c *Client) {
	var h header

	conn := c.c

	for {
		if err := binary.Read(conn, binary.BigEndian, &h); err != nil {
			c.shutdown(err)
			return
		}

		if h.Version != fcgiVersion {
			c.shutdown(errors.New("cgi: invalid fcgi version"))
			return
		}

		buf := make([]byte, int(h.ContentLength)+int(h.PaddingLength))

		if _, err := io.ReadFull(conn, buf); err != nil {
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
			r.done <- struct{}{}
			c.unsub(h.ID)
			r.cw <- nil
		default:
			c.shutdown(fmt.Errorf("unexpected type: %d", h.Type))
			return
		}
	}
}

func applyOptions(opts *Options, fns []DialOption) error {
	for _, fn := range fns {
		if err := fn(opts); err != nil {
			return err
		}
	}
	return nil
}

// Dial creates a new client and attempt to connect.
func Dial(network, addr string, options ...DialOption) (*Client, error) {
	opts := Options{
		dialTimeout: time.Minute,
		timeout:     time.Minute,
	}

	if err := applyOptions(&opts, options); err != nil {
		return nil, err
	}

	con, err := net.DialTimeout(network, addr, opts.dialTimeout)
	if err != nil {
		return nil, err
	}

	c := &Client{
		c: &conn{
			Conn:    con,
			timeout: opts.timeout,
		},
		sm: map[uint16]*Request{},
	}

	go receive(c)

	return c, nil
}

// WithDialTimeout creates a DialOption that sets the dial timeout.
// The default is 60 seconds.
func WithDialTimeout(timeout time.Duration) DialOption {
	return func(o *Options) error {
		o.dialTimeout = timeout
		return nil
	}
}

// WithTimeout creates a DialOption that sets the timeout for the client dispatching a record
// to the FCGI backend. The default is 60 seconds.
func WithTimeout(timeout time.Duration) DialOption {
	return func(o *Options) error {
		o.timeout = timeout
		return nil
	}
}
