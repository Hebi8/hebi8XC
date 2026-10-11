// Package github is the small part of the GitHub API that hebi8XC needs:
// the device flow of its GitHub App, and reading and adding SSH signing keys.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ClientID identifies the hebi8XC GitHub App. It is a public value: the
// device flow needs no client secret.
const ClientID = "Iv23liTrm1TRc0ZbMP1g"

const (
	apiVersion = "2022-11-28"
	maxBody    = 1 << 20
	perPage    = 100
	maxPages   = 10
)

// Client talks to GitHub. API and Web are the two base URLs.
type Client struct {
	API  string
	Web  string
	HTTP *http.Client
}

func New() *Client {
	return &Client{
		API:  "https://api.github.com",
		Web:  "https://github.com",
		HTTP: &http.Client{Timeout: 15 * time.Second},
	}
}

// Account is a GitHub account. The ID never changes; the login can.
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// User returns the user id that names this account in a roster and in the
// network tag.
func (a Account) User() string {
	return "github:" + strconv.FormatInt(a.ID, 10)
}

// AccountID is the inverse of Account.User.
func AccountID(user string) (int64, bool) {
	rest, ok := strings.CutPrefix(user, "github:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

// SigningKey is an SSH signing key registered on an account.
type SigningKey struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Title string `json:"title"`
}

// StatusError is an answer from the API other than success.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// Viewer returns the account a token belongs to.
func (c *Client) Viewer(ctx context.Context, token string) (Account, error) {
	var a Account
	err := c.api(ctx, http.MethodGet, "/user", token, nil, &a)
	return a, err
}

// AccountByID looks an account up by its durable id. It needs no token.
func (c *Client) AccountByID(ctx context.Context, id int64) (Account, error) {
	var a Account
	err := c.api(ctx, http.MethodGet, "/user/"+strconv.FormatInt(id, 10), "", nil, &a)
	return a, err
}

// SigningKeys lists the public SSH signing keys of login. It needs no token.
func (c *Client) SigningKeys(ctx context.Context, login string) ([]SigningKey, error) {
	return c.listKeys(ctx, "/users/"+url.PathEscape(login)+"/ssh_signing_keys", "")
}

// ViewerSigningKeys lists the SSH signing keys of the token's account.
func (c *Client) ViewerSigningKeys(ctx context.Context, token string) ([]SigningKey, error) {
	return c.listKeys(ctx, "/user/ssh_signing_keys", token)
}

// AddSigningKey registers key on the token's account.
func (c *Client) AddSigningKey(ctx context.Context, token, title, key string) error {
	in := map[string]string{"title": title, "key": key}
	return c.api(ctx, http.MethodPost, "/user/ssh_signing_keys", token, in, nil)
}

func (c *Client) listKeys(ctx context.Context, path, token string) ([]SigningKey, error) {
	var all []SigningKey
	for page := 1; page <= maxPages; page++ {
		var keys []SigningKey
		q := fmt.Sprintf("?per_page=%d&page=%d", perPage, page)
		if err := c.api(ctx, http.MethodGet, path+q, token, nil, &keys); err != nil {
			return nil, err
		}
		all = append(all, keys...)
		if len(keys) < perPage {
			return all, nil
		}
	}
	// A list cut short must not pass for the whole list: a node key on a
	// later page would silently drop out of the roster.
	return nil, fmt.Errorf("the account has more than %d SSH signing keys, more than h8c reads", maxPages*perPage)
}

func (c *Client) api(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		hc = c.noRedirects()
	}
	resp, err := hc.Do(req)
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
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func statusError(resp *http.Response, body []byte) error {
	var msg struct {
		Message string `json:"message"`
	}
	json.Unmarshal(body, &msg)
	e := &StatusError{Status: resp.StatusCode, Message: msg.Message}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	// Requests without a token share 60 per hour per IP address.
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		e.Message = "rate limit reached"
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			e.Message += ", resets at " + time.Unix(reset, 0).Format(time.TimeOnly)
		}
	}
	return e
}

// noRedirects returns the HTTP client for requests that carry a token or
// lead to one. net/http keeps the Authorization header when it follows a
// redirect to a subdomain, or to the same host over plain HTTP, so such a
// request treats any redirect as a failure instead.
func (c *Client) noRedirects() *http.Client {
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &hc
}

// IsNotFound reports whether err is GitHub saying the thing does not exist.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}
