//go:build darwin

package install

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

func TestWritePoolLoop_FullRangeOneLine(t *testing.T) {
	ips := make([]net.IP, 0, 255)
	for i := 1; i <= 255; i++ {
		ips = append(ips, net.IPv4(127, 50, 0, byte(i)))
	}
	var b bytes.Buffer
	writePoolLoop(&b, "lo0", "alias", ips)
	got := b.String()
	want := "for i in $(seq 1 255); do ifconfig lo0 alias 127.50.0.$i; done\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestWritePoolLoop_PartialTail(t *testing.T) {
	ips := []net.IP{
		net.IPv4(127, 50, 0, 100),
		net.IPv4(127, 50, 0, 101),
		net.IPv4(127, 50, 0, 102),
	}
	var b bytes.Buffer
	writePoolLoop(&b, "lo0", "-alias", ips)
	got := b.String()
	if !strings.Contains(got, "$(seq 100 102)") {
		t.Errorf("expected seq 100 102; got: %s", got)
	}
	if !strings.Contains(got, "ifconfig lo0 -alias 127.50.0.$i") {
		t.Errorf("expected -alias op + prefix; got: %s", got)
	}
}

func TestWritePoolLoop_EmptyIsNoOp(t *testing.T) {
	var b bytes.Buffer
	writePoolLoop(&b, "lo0", "alias", nil)
	if b.Len() != 0 {
		t.Errorf("expected no output for empty IPs; got: %q", b.String())
	}
}
