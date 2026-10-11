// Package control is the local interface between the running node and the
// tools of the same user: HTTP over a Unix socket that only the user can
// open. Listen and Dial are the two places to change when a platform needs a
// different transport.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
)

// ErrNotRunning means nothing is listening on the control socket.
var ErrNotRunning = errors.New("the node is not running (start it with: h8c up)")

// Listen opens the control socket at path, replacing one left behind by a
// dead node. The caller must hold the data directory's lock, which is what
// makes the leftover known to be dead.
func Listen(path string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Client calls the node behind a control socket.
type Client struct {
	http *http.Client
}

func Dial(path string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}}
}

// Get decodes the JSON answer of GET path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, out)
}

// Post sends an empty POST to path.
func (c *Client) Post(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodPost, path, nil)
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://h8c"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
