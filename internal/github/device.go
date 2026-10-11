package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeviceCode is what GitHub hands out at the start of the device flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"` // seconds
	Interval        int    `json:"interval"`   // seconds between polls
}

var (
	// ErrExpired means the user did not enter the code in time.
	ErrExpired = errors.New("the code expired before it was entered")
	// ErrDenied means the user cancelled the login on GitHub.
	ErrDenied = errors.New("the login was cancelled on GitHub")
)

// RequestDeviceCode starts the device flow.
func (c *Client) RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	var dc DeviceCode
	err := c.form(ctx, "/login/device/code", url.Values{"client_id": {ClientID}}, &dc)
	if err != nil {
		return nil, err
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, errors.New("GitHub sent no device code")
	}
	return &dc, nil
}

// WaitForToken polls until the user has entered the code and returns the
// user access token. wait is called before every poll with the time GitHub
// asks to leave between polls; Sleep is the real one.
func (c *Client) WaitForToken(ctx context.Context, dc *DeviceCode, wait func(context.Context, time.Duration) error) (string, error) {
	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	lifetime := time.Duration(dc.ExpiresIn) * time.Second
	form := url.Values{
		"client_id":   {ClientID},
		"device_code": {dc.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	for waited := time.Duration(0); ; {
		if err := wait(ctx, interval); err != nil {
			return "", err
		}
		waited += interval

		var answer struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
			Description string `json:"error_description"`
			Interval    int    `json:"interval"`
		}
		if err := c.form(ctx, "/login/oauth/access_token", form, &answer); err != nil {
			return "", err
		}
		switch answer.Error {
		case "":
			if answer.AccessToken == "" {
				return "", errors.New("GitHub sent no access token")
			}
			return answer.AccessToken, nil
		case "authorization_pending":
		case "slow_down":
			// The answer carries the new interval; it is 5 s more.
			interval += 5 * time.Second
			if next := time.Duration(answer.Interval) * time.Second; next > interval {
				interval = next
			}
		case "expired_token":
			return "", ErrExpired
		case "access_denied":
			return "", ErrDenied
		default:
			return "", fmt.Errorf("GitHub refused the login: %s: %s", answer.Error, answer.Description)
		}
		// Do not rely on GitHub alone to end the wait.
		if lifetime > 0 && waited >= lifetime {
			return "", ErrExpired
		}
	}
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// form posts to one of the two login endpoints, which answer errors with
// status 200 and an "error" field.
func (c *Client) form(ctx context.Context, path string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Web+path, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return statusError(resp, raw)
	}
	return json.Unmarshal(raw, out)
}
