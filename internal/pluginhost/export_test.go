package pluginhost

import (
	"errors"
	"io"

	"github.com/infrena/infrena/pkg/pluginproto"
	"github.com/infrena/infrena/pkg/pluginsdk"
	"github.com/infrena/infrena/pkg/provider"
)

// Test-only seams, so trust_test.go can build a half-connected client for the
// handshake cases without exporting any of this from the package.

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

func serveFor(p provider.Plugin, r io.Reader, w io.Writer) error { return pluginsdk.Serve(p, r, w) }

func newTestClient(w io.WriteCloser) *Client {
	return &Client{
		w:       w,
		pending: map[uint64]chan pluginproto.Response{},
		done:    make(chan struct{}),
	}
}

func errorsNew(s string) error { return errors.New(s) }

// setMaxMessage lowers the protocol line limit for one test and returns what
// restores it.
func setMaxMessage(n int) func() {
	old := maxMessage
	maxMessage = n
	return func() { maxMessage = old }
}
