// Package client talks to a Zymple server: over HTTPS with an app token from
// CI or a laptop, or over the unix socket on the host itself.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gokaykucuk/zymple/internal/deploy"
)

// Client is a connection to one Zymple server.
type Client struct {
	// Base is the URL prefix, without a trailing slash.
	Base  string
	Token string
	HTTP  *http.Client
}

// New returns a client for a network host such as https://deploy.example.com.
func New(host, token string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("host %q must be an http(s) URL", host)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, errors.New("plain http is only allowed for localhost; the token would travel unencrypted")
	}
	if token == "" {
		return nil, errors.New("a token is required (set ZYMPLE_TOKEN)")
	}
	return &Client{Base: strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), Token: token, HTTP: &http.Client{CheckRedirect: noRedirect}}, nil
}

// noRedirect stops the client at a redirect instead of following it. Zymple
// never redirects, and following one would send the token along: to the same
// host over plain http, for one.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewUnix returns a client for the operator socket on the host.
func NewUnix(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{Base: "http://zymple", HTTP: &http.Client{Transport: transport}}
}

// RegistryHost returns the host[:port] images are pushed to.
func (c *Client) RegistryHost() string {
	u, err := url.Parse(c.Base)
	if err != nil {
		return ""
	}
	return u.Host
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	return resp, nil
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		return fmt.Errorf("%s (HTTP %d)", body.Error, resp.StatusCode)
	}
	return fmt.Errorf("server answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}

// JSON performs a request and decodes a JSON answer into out.
func (c *Client) JSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Stream performs a request whose answer is a stream of events, calling
// onEvent for each. It returns an error if the operation reports failure or
// the stream ends without an outcome.
func (c *Client) Stream(ctx context.Context, method, path string, body any, onEvent func(deploy.Event)) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e deploy.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return fmt.Errorf("unreadable event from server: %w", err)
		}
		if e.Step == "done" {
			if e.Status == "ok" {
				if e.Message != "" {
					onEvent(e)
				}
				return nil
			}
			return errors.New(e.Message)
		}
		onEvent(e)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("connection lost: %w; the operation continues on the server, check `zymple status`", err)
	}
	return errors.New("connection closed before the outcome arrived; the operation continues on the server, check `zymple status`")
}
