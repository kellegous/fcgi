package phpfpm

import (
	"bytes"
	"testing"

	"github.com/kellegous/fcgi"
)

func TestThis(t *testing.T) {
	p := MustStart(DefaultConfig)
	defer p.Shutdown()

	c, err := fcgi.Dial("tcp", p.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var bout, berr bytes.Buffer
	req, err := c.BeginRequest(map[string][]string{
		"SCRIPT_FILENAME": {"TestThis.php"},
		"REQUEST_METHOD":  {"GET"},
		"CONTENT_LENGTH":  {"0"},
	}, nil, &bout, &berr)
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Wait(); err != nil {
		t.Fatal(err)
	}

	t.Log(bout.String(), berr.String())
}
