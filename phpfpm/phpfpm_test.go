package phpfpm

import (
	"log"
	"testing"
	"time"
)

func TestThis(t *testing.T) {
	p := MustStart(DefaultConfig)
	time.Sleep(10 * time.Second)
	defer p.Shutdown()
	log.Println(p)
}
