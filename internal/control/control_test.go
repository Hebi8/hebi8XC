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
	for _, errno := range []error{syscall.EPERM, syscall.EACCES, syscall.ENOENT, syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.Errno(10061), syscall.Errno(10013)} {
		c := &Client{http: &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "unix", Err: errno}
		}}}}
		e := c.Get(context.Background(), "/v1/status", nil)
		if errno == syscall.EPERM || errno == syscall.EACCES || errno == syscall.Errno(10013) {
			if errors.Is(e, ErrNotRunning) || !strings.Contains(e.Error(), "sandbox") {
				t.Fatal(e)
			}
		} else if !errors.Is(e, ErrNotRunning) {
			t.Fatal(e)
		}
	}
}
