package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
)

func TestDialErrorsDistinguishSandbox(t *testing.T) {
	for _, errno := range []error{syscall.EPERM, syscall.EACCES, syscall.ENOENT, syscall.ECONNREFUSED, syscall.ETIMEDOUT} {
		c := &Client{http: &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "unix", Err: errno}
		}}}}
		e := c.Get(context.Background(), "/v1/status", nil)
		if errno == syscall.EPERM || errno == syscall.EACCES {
			if errors.Is(e, ErrNotRunning) || !strings.Contains(e.Error(), "sandbox") {
				t.Fatal(e)
			}
		} else if errno == syscall.ENOENT || errno == syscall.ECONNREFUSED {
			if !errors.Is(e, ErrNotRunning) {
				t.Fatal(e)
			}
		} else if !errors.Is(e, errno) {
			t.Fatal(e)
		}
	}
}
