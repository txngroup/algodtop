package daemon

import (
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"algod.top/internal/config"
)

func TestRemoteErr(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://agent/api/nodes", Err: io.ErrUnexpectedEOF}
	stderr := "kex_exchange_identification: read: Connection reset by peer\r\nConnection reset by 192.0.2.7 port 22"
	want := "unexpected EOF: kex_exchange_identification: read: Connection reset by peer; Connection reset by 192.0.2.7 port 22"
	if got := remoteErr(err, stderr); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := remoteErr(errors.New("agent /api/nodes: HTTP 401: unauthorized"), ""); got != "agent /api/nodes: HTTP 401: unauthorized" {
		t.Errorf("without stderr: %q", got)
	}
	if got := remoteErr(io.EOF, "same\nsame\n"); got != "EOF: same" {
		t.Errorf("repeated line: %q", got)
	}
}

func TestDownReason(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	r := &remoteRun{cfg: config.Remote{Name: "edge"}, started: now.Add(-90 * time.Second), lastErr: "connection refused"}
	if got := r.downReason(now); got != "agent edge unreachable for 1m30s: connection refused" {
		t.Errorf("never reached: %q", got)
	}
	r.lastOK, r.lastErr = now.Add(-48*time.Minute-400*time.Millisecond), ""
	if got := r.downReason(now); got != "agent edge unreachable for 48m0s: no answer" {
		t.Errorf("after a success: %q", got)
	}
}
