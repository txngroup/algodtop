package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A connection that never sends a request held http.Server.Shutdown for
// 5 s (issue #15). shutdown closes it at once, and still lets a request in
// flight finish.
func TestShutdownClosesFreshConn(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	srv := newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "done")
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)

	idle, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	got := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- string(b)
	}()
	<-started
	waitFresh(t, srv, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	done := make(chan error, 1)
	go func() { done <- srv.shutdown(ctx) }()
	time.Sleep(300 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("shutdown took %v with a connection that sent nothing", d)
	}
	if s := <-got; s != "done" {
		t.Errorf("request in flight got %q, want done", s)
	}
	idle.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Error("connection that sent nothing is still open")
	}
}

func waitFresh(t *testing.T, s *webServer, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		k := len(s.fresh)
		s.mu.Unlock()
		if k >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server saw fewer than %d new connections", n)
}
