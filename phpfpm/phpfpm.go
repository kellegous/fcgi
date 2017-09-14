package phpfpm

import (
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// DefaultConfig ...
var DefaultConfig = &Config{
	MaxChildren:     5,
	MinSpareServers: 1,
	MaxSpareServers: 3,
	StartServers:    3,
}

// Proc ...
type Proc struct {
	Addr    string
	Cfg     *Config
	Dir     string
	Process *os.Process
}

// Config ...
type Config struct {
	MaxChildren     int
	MinSpareServers int
	MaxSpareServers int
	StartServers    int
}

type configFile struct {
	*Config
	Addr     string
	ErrorLog string
}

func templateFromLines(src []string) (*template.Template, error) {
	return template.New("conf").Parse(strings.Join(src, "\n"))
}

func writeConfig(dst string, c *configFile) error {
	t, err := templateFromLines([]string{
		"[global]",
		"daemonize = no",
		"error_log = {{.ErrorLog}}",
		"[www]",
		"listen = {{.Addr}}",
		"pm = dynamic",
		"pm.max_children = {{.MaxChildren}}",
		"pm.min_spare_servers = {{.MinSpareServers}}",
		"pm.max_spare_servers = {{.MaxSpareServers}}",
		"pm.start_servers = {{.StartServers}}",
	})
	if err != nil {
		return err
	}

	w, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer w.Close()

	return t.Execute(w, c)
}

func localAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}

	if err := l.Close(); err != nil {
		return "", err
	}

	return l.Addr().String(), nil
}

// Shutdown ...
func (p *Proc) Shutdown() error {
	errA := p.Process.Kill()
	errB := os.RemoveAll(p.Dir)
	if errA != nil {
		return errA
	}
	return errB
}

// MustStart ...
func MustStart(cfg *Config) *Proc {
	p, err := Start(cfg)
	if err != nil {
		panic(err)
	}
	return p
}

func waitFor(addr string) error {
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

// Start ...
func Start(cfg *Config) (*Proc, error) {
	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, err
	}

	addr, err := localAddr()
	if err != nil {
		return nil, err
	}

	cf := filepath.Join(tmp, "conf")
	if err := writeConfig(cf, &configFile{
		Config:   cfg,
		Addr:     addr,
		ErrorLog: filepath.Join(tmp, "err"),
	}); err != nil {
		return nil, err
	}

	c := exec.Command("php-fpm", "-n", "-y", cf)
	c.Stderr = os.Stderr
	c.Stdout = os.Stdout

	if err := c.Start(); err != nil {
		return nil, err
	}

	if err := waitFor(addr); err != nil {
		return nil, err
	}

	return &Proc{
		Addr:    addr,
		Cfg:     cfg,
		Dir:     tmp,
		Process: c.Process,
	}, nil
}
