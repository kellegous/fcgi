package fcgi

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	std "net/http/fcgi"
	"os"
	"path/filepath"
	"testing"
)

func TestPHP(t *testing.T) {
	c, err := Dial("tcp", "192.168.42.101:8061")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	params := map[string]string{
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
		"SCRIPT_FILENAME": "/var/www/html/info.php",
	}

	req, err := c.BeginRequest(params, nil, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	if err := req.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestBasics(t *testing.T) {
	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	sck := filepath.Join(tmp, "sock")

	l, err := net.Listen("unix", sck)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		if err := std.Serve(l, http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, "Hello FCGI")
			})); err != nil {
			t.Fatal(err)
		}
	}()

	c, err := Dial("unix", sck)
	if err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		"REQUEST_METHOD":  "GET",
		"SERVER_PROTOCOL": "HTTP/1.1",
	}

	req, err := c.BeginRequest(params, nil, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	if err := req.Wait(); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}
