package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// HTTPHealth checks a web container over loopback HTTP.
type HTTPHealth struct {
	// Interval between attempts; defaults to 500ms.
	Interval time.Duration
	// Client defaults to one with a 5s timeout that does not follow redirects.
	Client *http.Client
}

// Wait polls until the path answers with a 2xx or 3xx status.
func (h HTTPHealth) Wait(ctx context.Context, port int, path string, timeout time.Duration) error {
	interval := h.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	client := h.Client
	if client == nil {
		client = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := errors.New("no attempt completed")
	for {
		if err := probe(ctx, client, url); err == nil {
			return nil
		} else if ctx.Err() == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check did not pass within %s: %v", timeout, last)
		case <-time.After(interval):
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// PortRange hands out free loopback ports from [Low, High].
type PortRange struct {
	Low, High int

	mu    sync.Mutex
	taken map[int]bool
	next  int
}

// Reserve marks ports as in use, for releases recorded before a restart.
func (p *PortRange) Reserve(ports ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.taken == nil {
		p.taken = map[int]bool{}
	}
	for _, port := range ports {
		p.taken[port] = true
	}
}

// Acquire returns a port that is neither reserved nor bound right now.
func (p *PortRange) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.taken == nil {
		p.taken = map[int]bool{}
	}
	size := p.High - p.Low + 1
	if size <= 0 {
		return 0, errors.New("port range is empty")
	}
	for i := 0; i < size; i++ {
		port := p.Low + (p.next+i)%size
		if p.taken[port] || !bindable(port) {
			continue
		}
		p.taken[port] = true
		p.next = (p.next + i + 1) % size
		return port, nil
	}
	return 0, fmt.Errorf("no free port between %d and %d", p.Low, p.High)
}

// Release returns a port to the pool.
func (p *PortRange) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.taken, port)
}

func bindable(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// SleepContext sleeps for d or until ctx is done.
func SleepContext(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
