package fcgi

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	std "net/http/fcgi"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
)

type mockServer struct {
	list net.Listener
	dir  string
	t    *testing.T
}

func (s *mockServer) Close() error {
	defer os.RemoveAll(s.dir)
	return s.list.Close()
}

func (s *mockServer) Serve(h http.Handler) {
	go func() {
		std.Serve(s.list, h)
	}()
}

func (s *mockServer) Network() string {
	return "unix"
}

func (s *mockServer) Addr() string {
	return filepath.Join(s.dir, "sock")
}

func newServer(t *testing.T) *mockServer {
	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(tmp, "sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	return &mockServer{
		list: l,
		dir:  tmp,
		t:    t,
	}
}

func mustHaveRequest(
	t *testing.T,
	out io.Reader,
	status int,
	hdrs map[string][]string,
	body string) {

	br := bufio.NewReader(out)

	mh, err := textproto.NewReader(br).ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}

	s, err := statusFromHeaders(http.Header(mh))
	if err != nil {
		t.Fatal(err)
	}

	if s != status {
		t.Fatalf("Expected status %d, got %d", status, s)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, br); err != nil {
		t.Fatal(err)
	}

	if buf.String() != body {
		t.Fatalf("expected body of:\n%s\ngot:%s\n", body, buf.String())
	}
}

func TestStatusOK(t *testing.T) {
	s := newServer(t)
	defer s.Close()

	s.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello FCGI")
	}))

	c, err := Dial(s.Network(), s.Addr())
	if err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
	}

	var bout, berr bytes.Buffer
	req, err := c.BeginRequest(params, nil, &bout, &berr)
	if err != nil {
		t.Fatal(err)
	}

	if err := req.Wait(); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	mustHaveRequest(t,
		&bout,
		http.StatusOK,
		nil,
		"Hello FCGI\n")
}

func TestStatusNotOK(t *testing.T) {
	s := newServer(t)
	defer s.Close()

	s.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "Oh No!")
	}))

	c, err := Dial(s.Network(), s.Addr())
	if err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
	}

	var bout, berr bytes.Buffer
	req, err := c.BeginRequest(params, nil, &bout, &berr)
	if err != nil {
		t.Fatal(err)
	}

	if err := req.Wait(); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	mustHaveRequest(t,
		&bout,
		http.StatusInternalServerError,
		nil,
		"Oh No!\n")
}
