package fcgi

import (
	"net"
	"sync"
)

// conn is a connection and its associated write lock that is used
// to enforce mutual exclusive access to the connection during writes.
type conn struct {
	l sync.Mutex
	net.Conn
}

// Write the bytes in p to the connection after acquiring the write
// lock.
func (c *conn) Write(p []byte) (int, error) {
	c.l.Lock()
	defer c.l.Unlock()
	return c.Conn.Write(p)
}

// Send a record over the connection for the given request, of the given
// type and containing the given buffer.
func (c *conn) send(id uint16, recType recType, w *buffer) error {
	w.WriteHeader(id, recType, w.Len())
	return w.SendTo(c)
}
