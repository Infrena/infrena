package pluginhost

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/infrena/infrena/pkg/pluginproto"
)

// A plugin whose stdout the host has stopped reading blocks on its next write,
// and a blocked plugin never exits, so Close, which waits for it to exit rather
// than kill it mid-Create, waits forever. Found by the GCP live suite: one
// discover reply over the line limit left `infrena discover` hung for 30
// minutes after it had already reported the failure.
//
// So once the stream is unusable, the host keeps reading and throws the bytes
// away. Each case writes a line the host cannot use and then keeps writing, the
// way a plugin with more replies in flight does. It fails by timing out,
// because the failure being tested is a hang.
func TestTheHostKeepsDrainingAPluginItCanNoLongerUnderstand(t *testing.T) {
	cases := []struct {
		name string
		bad  string
	}{
		// Past 64KiB too: a Scanner's limit is the larger of its max and
		// its starting buffer, so a line between the two is read fine.
		{"a line over the limit", strings.Repeat("x", 128*1024) + "\n"},
		{"a line that is not a protocol message", "hello from fmt.Println\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer setMaxMessage(1024)()

			hostR, pluginW := io.Pipe()
			c := newTestClient(nopWriteCloser{io.Discard})

			hello, _ := json.Marshal(pluginproto.Handshake{Protocol: pluginproto.Version, Name: "bad"})
			wrote := make(chan struct{})
			go func() {
				defer close(wrote)
				_, _ = pluginW.Write(append(hello, '\n'))
				_, _ = io.WriteString(pluginW, tc.bad)
				// More output after the bad line, which is what blocks.
				for range 64 {
					if _, err := io.WriteString(pluginW, `{"id":99}`+"\n"); err != nil {
						return
					}
				}
				_ = pluginW.Close()
			}()

			if err := c.start(hostR, "bad"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wrote:
			case <-time.After(5 * time.Second):
				t.Fatal("the plugin is still blocked writing: the host stopped reading its stdout")
			}
			select {
			case <-c.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the host never finished with a stream that had ended")
			}
			// What the user reads. "bufio.Scanner: token too long" is what
			// the reader saw, not what happened.
			if msg := c.exitMessage(c.closed); strings.Contains(msg, "bufio") {
				t.Errorf("the failure is reported as a Go error: %q", msg)
			}
		})
	}
}

// stderr is the same pipe problem from the other side. A log line over the
// stderr limit used to end the reader, and the plugin's next log write then
// blocked for good.
func TestAnOverlongLogLineDoesNotStopThePluginLogging(t *testing.T) {
	r, w := io.Pipe()
	tail := newStderrTail(5)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		forwardStderr(r, "bad", tail, nil)
	}()

	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = io.WriteString(w, strings.Repeat("x", 2*1024*1024)+"\n")
		_, _ = io.WriteString(w, "the line after\n")
		_ = w.Close()
	}()

	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the plugin is still blocked writing to stderr")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the stderr reader never finished")
	}
	// Dropping the rest of the log is survivable; dropping it silently is not,
	// since the tail is what a crash message quotes.
	if !strings.Contains(tail.String(), "dropped") {
		t.Errorf("nothing in the log says a line was dropped: %q", tail.String())
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
