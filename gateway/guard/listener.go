package guard

import (
	"net"
	"sync"
)

// Listener bounds a listener's connections: Max open at once (Accept waits
// for one to close, leaving the rest in the kernel's backlog), and
// PerSource from one remote address (a connection past it is closed as it
// is accepted). 0: no bound. Behind a sidecar, every connection is the
// sidecar's: PerSource is for a gateway reached directly.
type Listener struct {
	net.Listener
	Max, PerSource int

	once   sync.Once
	slots  chan struct{}
	done   chan struct{} // closed by Close: an Accept waiting for a slot returns
	closed sync.Once
	mu     sync.Mutex
	byHost map[string]int
}

// Accept returns the next connection within the bounds.
func (l *Listener) Accept() (net.Conn, error) {
	l.init()
	for {
		if l.slots != nil {
			select {
			case l.slots <- struct{}{}:
			case <-l.done:
				return nil, net.ErrClosed
			}
		}
		c, err := l.Listener.Accept()
		if err != nil {
			l.free("")
			return nil, err
		}
		host := ""
		if l.PerSource > 0 {
			host, _, _ = net.SplitHostPort(c.RemoteAddr().String())
			l.mu.Lock()
			over := l.byHost[host] >= l.PerSource
			if !over {
				l.byHost[host]++
			}
			l.mu.Unlock()
			if over {
				c.Close()
				if l.slots != nil {
					<-l.slots
				}
				continue
			}
		}
		return &conn{Conn: c, l: l, host: host}, nil
	}
}

func (l *Listener) init() {
	l.once.Do(func() {
		if l.Max > 0 {
			l.slots = make(chan struct{}, l.Max)
		}
		l.byHost = map[string]int{}
		l.done = make(chan struct{})
	})
}

// Close closes the listener, and ends an Accept waiting for a slot (a
// shutdown must not wait for a connection to close first).
func (l *Listener) Close() error {
	l.init()
	l.closed.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// free returns a connection's slot and its host's count.
func (l *Listener) free(host string) {
	if host != "" {
		l.mu.Lock()
		if l.byHost[host]--; l.byHost[host] <= 0 {
			delete(l.byHost, host)
		}
		l.mu.Unlock()
	}
	if l.slots != nil {
		<-l.slots
	}
}

type conn struct {
	net.Conn
	l    *Listener
	host string
	once sync.Once
}

func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.free(c.host) })
	return err
}
