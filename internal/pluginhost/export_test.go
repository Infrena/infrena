package pluginhost

import (
	"errors"
	"io"

	"github.com/infrata/infrata/pkg/pluginproto"
	"github.com/infrata/infrata/pkg/pluginsdk"
	"github.com/infrata/infrata/pkg/provider"
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
