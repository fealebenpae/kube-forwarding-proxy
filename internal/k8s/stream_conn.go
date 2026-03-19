package k8s

import (
	"bytes"
	"io"
	"net"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

// streamConn adapts a pair of SPDY httpstream.Streams into a net.Conn.
// The data stream carries application bytes; the error stream relays
// any apiserver-side error messages.
//
// spdyCon is the owning SPDY connection; pass nil when the connection is
// managed by a pool — in that case supply an onClose callback instead.
// onClose is called after the streams are reset, before spdyCon (if any) is
// closed; the pool uses it to decrement the reference count.
type streamConn struct {
	data    httpstream.Stream
	errStr  httpstream.Stream
	spdyCon httpstream.Connection // nil when conn is pooled
	onClose func()                // nil-safe; called by Close before spdyCon teardown
}

// newStreamConn creates a streamConn and starts a background goroutine that
// drains errStr, logging any non-empty message it receives.
func newStreamConn(
	data httpstream.Stream,
	errStr httpstream.Stream,
	spdyCon httpstream.Connection,
	onClose func(),
	logger *zap.SugaredLogger,
	podName string,
) *streamConn {
	sc := &streamConn{data: data, errStr: errStr, spdyCon: spdyCon, onClose: onClose}

	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, errStr); err != nil && err != io.EOF {
			logger.Warnw("reading SPDY error stream", "error", err)
		}
		if buf.Len() > 0 {
			logger.Warnw("port-forward error from apiserver",
				"pod", podName,
				"message", buf.String(),
			)
		}
	}()

	return sc
}

func (c *streamConn) Read(b []byte) (int, error)  { return c.data.Read(b) }
func (c *streamConn) Write(b []byte) (int, error) { return c.data.Write(b) }

// CloseWrite closes the write side of the SPDY data stream, sending an EOF
// to the remote without tearing down the connection. This allows the remote
// side to drain its buffer and close gracefully.
func (c *streamConn) CloseWrite() error { return c.data.Close() }

// Close resets both SPDY streams, fires the onClose callback (if set), and
// closes the underlying SPDY connection (if not managed by a pool).
func (c *streamConn) Close() error {
	_ = c.data.Reset()
	_ = c.errStr.Reset()
	if c.onClose != nil {
		c.onClose()
	}
	if c.spdyCon != nil {
		return c.spdyCon.Close()
	}
	return nil
}

// LocalAddr and RemoteAddr return zero-value *net.TCPAddr so that consumers
// that type-assert net.Conn.LocalAddr() to *net.TCPAddr (e.g. armon/go-socks5)
// do not panic. The IP must be a valid (non-nil) address so that the go-socks5
// address formatter does not error when encoding the SOCKS5 reply; 0.0.0.0
// is an acceptable placeholder for a stream with no real network address.
func (c *streamConn) LocalAddr() net.Addr  { return &net.TCPAddr{IP: net.IPv4zero} }
func (c *streamConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

// SetDeadline and friends are no-ops; SPDY streams don't support per-stream
// deadlines. Use ForwardManager.SetIdleTimeout on the connection if needed.
func (c *streamConn) SetDeadline(_ time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(_ time.Time) error { return nil }
