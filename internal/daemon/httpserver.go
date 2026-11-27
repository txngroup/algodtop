package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// webServer is an http.Server that remembers its connections that have
// not sent a request yet.
type webServer struct {
	*http.Server
	mu    sync.Mutex
	fresh map[net.Conn]struct{}
}

func newHTTPServer(h http.Handler) *webServer {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	// The unix socket also accepts HTTP/2 without TLS for the SSH relay.
	p.SetUnencryptedHTTP2(true)
	srv.Protocols = p
	w := &webServer{Server: srv, fresh: map[net.Conn]struct{}{}}
	srv.ConnState = w.track
	return w
}

func (w *webServer) track(c net.Conn, s http.ConnState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s == http.StateNew {
		w.fresh[c] = struct{}{}
	} else {
		delete(w.fresh, c)
	}
}

// shutdown stops the server and waits for requests in flight. Shutdown
// treats a connection that never sent a request as idle only once it is
// 5 s old. A browser's preconnect or an HTTP client's spare dial leaves
// one, so shutdown closes those connections itself.
func (w *webServer) shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- w.Shutdown(ctx) }()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-t.C:
			w.mu.Lock()
			for c := range w.fresh {
				c.Close()
				delete(w.fresh, c)
			}
			w.mu.Unlock()
		}
	}
}

// shutdownAll stops every server at once, so one slow server does not
// spend the others' share of the deadline.
func shutdownAll(ctx context.Context, servers []*webServer) {
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() { defer wg.Done(); s.shutdown(ctx) }()
	}
	wg.Wait()
}
