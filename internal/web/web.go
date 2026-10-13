// Package web is the node's page for a browser on the same machine: the
// sessions of all nodes, to list and to read. It serves a few static files
// and, under /api, the same read-only answers the node gives its command
// line tools.
//
// What it shows is private, and it comes from session files and from other
// nodes, so it is not to be trusted either. Three things follow:
//
//   - Any page in the browser can send requests to a port on this machine,
//     and with DNS rebinding can make them look same-origin. So a request
//     must name this server by a loopback address in its Host header, and
//     must carry the token: in a cookie the browser only sends from this
//     site itself, or in an Authorization header. No answer ever allows
//     another origin to read it.
//   - The token gets into the browser through a link from "h8c web". The
//     server trades it for the cookie and sends the browser on to an
//     address without it.
//   - Session text must never run. The page writes text as text, and the
//     Content-Security-Policy allows nothing but this server's own script
//     and style: no inline script, no other origin, no frames.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

//go:embed static
var static embed.FS

const (
	cookieName = "h8c_web"
	// The cookie lasts a month; "h8c web" gives a new link any time.
	cookieAge = 30 * 24 * 3600

	csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
		"img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// hosts are the names a browser on this machine reaches the page by. The
// page listens on one of them, and a request has to name one of them.
var hosts = []string{"127.0.0.1", "localhost", "[::1]"}

// LoopbackAddr checks that addr, a host:port to listen on, is on this
// machine only: the page is for a browser here, never for another device.
// Other loopback addresses, such as 127.0.0.2, are not accepted either:
// the page would listen there and then refuse every request by its Host.
func LoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--web %q: want HOST:PORT", addr)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if !slices.Contains(hosts, host) {
		return fmt.Errorf("--web %q: the web interface only listens on this machine itself; use 127.0.0.1, [::1] or localhost", addr)
	}
	return nil
}

// LoadToken reads the token that opens the web interface from path, making
// one first if there is none. The file is for this user only.
func LoadToken(path string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		token := hex.EncodeToString(raw)
		_, err = f.WriteString(token + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return token, err
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return ReadToken(path)
}

// ReadToken reads the token from path.
func ReadToken(path string) (string, error) {
	have, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(have))
	if len(token) < 32 {
		return "", fmt.Errorf("%s does not hold a token", path)
	}
	return token, nil
}

// URL is the link that opens the web interface at addr in a browser.
func URL(addr, token string) string {
	return "http://" + addr + "/?token=" + token
}

type handler struct {
	api   http.Handler
	token string
	hosts map[string]bool
	files http.Handler
}

// New returns the handler of the web interface. api answers the read-only
// JSON routes, which appear under /api; token opens the interface; port is
// the port it listens on, which requests have to name.
func New(api http.Handler, token string, port int) http.Handler {
	files, _ := fs.Sub(static, "static")
	h := &handler{api: http.StripPrefix("/api", api), token: token, hosts: make(map[string]bool),
		files: http.FileServerFS(files)}
	for _, host := range hosts {
		h.hosts[host+":"+strconv.Itoa(port)] = true
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A request that does not name this server by a loopback address was
	// sent by a page that reached it under another name.
	if !h.hosts[r.Host] {
		http.Error(w, "this address is not the h8c web interface", http.StatusForbidden)
		return
	}
	head := w.Header()
	head.Set("Content-Security-Policy", csp)
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("X-Frame-Options", "DENY")
	head.Set("Referrer-Policy", "no-referrer")
	head.Set("Cross-Origin-Resource-Policy", "same-origin")
	head.Set("Cross-Origin-Opener-Policy", "same-origin")
	head.Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "the web interface only answers GET", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case r.URL.Path == "/":
		h.front(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		if !h.allowed(r) {
			http.Error(w, "run h8c web in a terminal for a link that opens this", http.StatusUnauthorized)
			return
		}
		h.api.ServeHTTP(w, r)
	case r.URL.Path == "/app.js", r.URL.Path == "/app.css":
		h.files.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// front answers "/": the page for a browser that holds the cookie, the
// trade of a token for the cookie, and for everyone else a note that says
// how to get in.
func (h *handler) front(w http.ResponseWriter, r *http.Request) {
	if token := r.URL.Query().Get("token"); token != "" {
		if !h.is(token) {
			h.notice(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: h.token, Path: "/", MaxAge: cookieAge,
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		// On to the address without the token: it does not stay in the
		// address bar, and the page never sees it.
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !h.allowed(r) {
		h.notice(w, r)
		return
	}
	h.page(w, "index.html", http.StatusOK)
}

// notice answers a browser that may not see the page with how to get in,
// in Chinese if it would rather read that than English.
func (h *handler) notice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Accept-Language")
	name := "notice.html"
	if prefersChinese(r.Header.Get("Accept-Language")) {
		name = "notice.zh.html"
	}
	h.page(w, name, http.StatusUnauthorized)
}

// prefersChinese reads an Accept-Language header, such as
// "zh-CN,zh;q=0.9,en;q=0.8": of Chinese and English, is Chinese the one
// with the higher weight, or at the same weight the one named first?
// Without either it is not; the notice is then in English.
func prefersChinese(accept string) bool {
	best, chinese := 0.0, false
	for _, part := range strings.Split(accept, ",") {
		lang, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		lang = strings.ToLower(strings.TrimSpace(lang))
		zh := lang == "zh" || strings.HasPrefix(lang, "zh-")
		if !zh && lang != "en" && !strings.HasPrefix(lang, "en-") {
			continue
		}
		weight := 1.0
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			var err error
			if weight, err = strconv.ParseFloat(q, 64); err != nil {
				continue
			}
		}
		if weight > best {
			best, chinese = weight, zh
		}
	}
	return chinese
}

func (h *handler) page(w http.ResponseWriter, name string, status int) {
	raw, err := static.ReadFile("static/" + name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(raw)
}

func (h *handler) is(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

// allowed reports whether a request carries the token, as the cookie or as
// a bearer token.
func (h *handler) allowed(r *http.Request) bool {
	if c, err := r.Cookie(cookieName); err == nil && h.is(c.Value) {
		return true
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && h.is(bearer)
}
