package deploy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func serverPort(t *testing.T, h http.Handler) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func TestHTTPHealth(t *testing.T) {
	h := HTTPHealth{Interval: 5 * time.Millisecond}
	var hits atomic.Int64
	port := serverPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
		case "/redirect":
			http.Redirect(w, r, "http://unreachable.invalid/", http.StatusFound)
		case "/warming":
			if hits.Add(1) < 4 {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	if err := h.Wait(ctx, port, "/ok", time.Second); err != nil {
		t.Errorf("healthy endpoint: %v", err)
	}
	if err := h.Wait(ctx, port, "/redirect", time.Second); err != nil {
		t.Errorf("a redirect counts as healthy and must not be followed: %v", err)
	}
	if err := h.Wait(ctx, port, "/warming", 2*time.Second); err != nil || hits.Load() < 4 {
		t.Errorf("should retry until healthy: %v after %d hits", err, hits.Load())
	}
	err := h.Wait(ctx, port, "/broken", 60*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Errorf("unhealthy endpoint: %v", err)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if err := (HTTPHealth{}).Wait(ctx, closed, "/", 30*time.Millisecond); err == nil {
		t.Error("nothing is listening, want an error")
	}
}

func TestPortRange(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	bound := l.Addr().(*net.TCPAddr).Port

	p := &PortRange{Low: bound, High: bound}
	if _, err := p.Acquire(); err == nil {
		t.Fatal("a port that is bound must not be handed out")
	}

	// Find three free neighbours for a deterministic range.
	base := 0
	for candidate := 30000; candidate < 40000; candidate += 7 {
		if bindable(candidate) && bindable(candidate+1) && bindable(candidate+2) {
			base = candidate
			break
		}
	}
	if base == 0 {
		t.Skip("no free ports found")
	}
	p = &PortRange{Low: base, High: base + 2}
	p.Reserve(base)
	a, _ := p.Acquire()
	b, _ := p.Acquire()
	if a == b || a == base || b == base {
		t.Fatalf("got %d and %d from a range with %d reserved", a, b, base)
	}
	if _, err := p.Acquire(); err == nil {
		t.Fatal("the range is exhausted")
	}
	p.Release(a)
	if c, err := p.Acquire(); err != nil || c != a {
		t.Fatalf("released port not reused: %d, %v", c, err)
	}
	if _, err := (&PortRange{Low: 5, High: 4}).Acquire(); err == nil {
		t.Fatal("an empty range must fail")
	}
}

func TestSleepContext(t *testing.T) {
	start := time.Now()
	SleepContext(ctx, 0)
	SleepContext(ctx, 20*time.Millisecond)
	if time.Since(start) < 20*time.Millisecond {
		t.Error("did not sleep")
	}
	cancelled, cancel := contextWithCancel()
	cancel()
	start = time.Now()
	SleepContext(cancelled, time.Minute)
	if time.Since(start) > time.Second {
		t.Error("did not stop when the context ended")
	}
}
