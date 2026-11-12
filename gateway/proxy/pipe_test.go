package proxy

import (
	"io"
	"time"
)

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// syncWait waits a moment for the server's handler goroutine to observe the
// client's cancellation over a real connection.
func syncWait() { time.Sleep(5 * time.Millisecond) }
