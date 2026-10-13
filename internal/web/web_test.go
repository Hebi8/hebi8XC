package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const (
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPort  = 4893
	goodHost  = "127.0.0.1:4893"
)

// api stands in for the node: it says which path it was asked.
var api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"asked":"` + r.URL.Path + `"}`))
})

type request struct {
	method, path, host string
	cookie, bearer     string
	origin             string
}

func do(t *testing.T, req request) *httptest.ResponseRecorder {
	t.Helper()
	if req.method == "" {
		req.method = http.MethodGet
	}
	r := httptest.NewRequest(req.method, req.path, nil)
	r.Host = goodHost
	if req.host != "" {
		r.Host = req.host
	}
	if req.cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: req.cookie})
	}
	if req.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+req.bearer)
	}
	if req.origin != "" {
		r.Header.Set("Origin", req.origin)
	}
	w := httptest.NewRecorder()
	New(api, testToken, testPort).ServeHTTP(w, r)
	return w
}

func TestWithoutCredentialsThereIsOnlyTheNotice(t *testing.T) {
	front := do(t, request{path: "/"})
	if front.Code != http.StatusUnauthorized || !strings.Contains(front.Body.String(), "h8c web") {
		t.Errorf("GET /: %d\n%s", front.Code, front.Body)
	}
	if strings.Contains(front.Body.String(), "app.js") {
		t.Error("the notice loads the application script")
	}
	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/messages?node=a&key=b", "/api/", "/api/v1/anything"} {
		for _, req := range []request{{path: path}, {path: path, cookie: "wrong"}, {path: path, bearer: "wrong"},
			{path: path, cookie: testToken[:len(testToken)-1]}} {
			w := do(t, req)
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "asked") {
				t.Errorf("%+v: %d %s", req, w.Code, w.Body)
			}
		}
	}
}

func TestTokenIsTradedForACookie(t *testing.T) {
	w := do(t, request{path: "/?token=" + testToken})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("GET /?token=...: %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if strings.Contains(w.Body.String()+w.Header().Get("Location"), testToken) {
		t.Error("the answer repeats the token outside the cookie")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != cookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge <= 0 {
		t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, for this host only", c)
	}

	// With the cookie: the page, and the API.
	page := do(t, request{path: "/", cookie: c.Value})
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `<script src="/app.js">`) {
		t.Errorf("GET / with the cookie: %d", page.Code)
	}
	if strings.Contains(page.Body.String(), testToken) {
		t.Error("the page contains the token")
	}
	if got := do(t, request{path: "/api/v1/sessions", cookie: c.Value}); got.Code != http.StatusOK ||
		got.Body.String() != `{"asked":"/v1/sessions"}` {
		t.Errorf("API with the cookie: %d %s", got.Code, got.Body)
	}
	// A program sends the token itself.
	if got := do(t, request{path: "/api/v1/sessions", bearer: testToken}); got.Code != http.StatusOK {
		t.Errorf("API with a bearer token: %d", got.Code)
	}

	bad := do(t, request{path: "/?token=" + strings.Repeat("0", 64)})
	if bad.Code != http.StatusUnauthorized || len(bad.Result().Cookies()) != 0 {
		t.Errorf("a wrong token: %d, cookies %v", bad.Code, bad.Result().Cookies())
	}
}

// A page elsewhere can make the browser talk to this port, and by DNS
// rebinding under a name of its own. Such a request names another host.
func TestOnlyLoopbackNamesAreAnswered(t *testing.T) {
	for _, host := range []string{"127.0.0.1:4893", "localhost:4893", "[::1]:4893"} {
		if w := do(t, request{path: "/api/v1/sessions", host: host, cookie: testToken}); w.Code != http.StatusOK {
			t.Errorf("Host %q: %d", host, w.Code)
		}
	}
	for _, host := range []string{"evil.example:4893", "evil.example", "127.0.0.1", "127.0.0.1:80", "localhost:4892",
		"localhost.evil.example:4893", "127.0.0.1.evil.example:4893", "192.168.1.20:4893", "0.0.0.0:4893", "[::]:4893"} {
		for _, path := range []string{"/", "/api/v1/sessions", "/app.js", "/?token=" + testToken} {
			w := do(t, request{path: path, host: host, cookie: testToken, bearer: testToken})
			if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "asked") {
				t.Errorf("Host %q, GET %s: %d %s", host, path, w.Code, w.Body)
			}
		}
	}
}

func TestNoAnswerAllowsAnotherOrigin(t *testing.T) {
	for _, req := range []request{
		{path: "/", cookie: testToken},
		{path: "/api/v1/sessions", cookie: testToken, origin: "https://evil.example"},
		{path: "/api/v1/sessions", origin: "https://evil.example"},
		{path: "/app.js", origin: "https://evil.example"},
		{method: http.MethodOptions, path: "/api/v1/sessions", origin: "https://evil.example"},
		{path: "/?token=" + testToken, origin: "https://evil.example"},
	} {
		w := do(t, req)
		for name := range w.Header() {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("%+v: header %s: %s", req, name, w.Header().Get(name))
			}
		}
	}
}

func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	for _, req := range []request{
		{path: "/"}, {path: "/", cookie: testToken}, {path: "/?token=" + testToken},
		{path: "/api/v1/sessions"}, {path: "/api/v1/sessions", cookie: testToken},
		{path: "/app.js"}, {path: "/app.css"}, {path: "/nothing-here"},
		{method: http.MethodPost, path: "/api/v1/sessions", cookie: testToken},
	} {
		w := do(t, req)
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'",
			"frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%+v: CSP %q lacks %q", req, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe") || strings.Contains(csp, "*") || strings.Contains(csp, "data:") {
			t.Errorf("%+v: CSP %q allows too much", req, csp)
		}
		for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
			"Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "Cross-Origin-Resource-Policy": "same-origin"} {
			if h.Get(name) != want {
				t.Errorf("%+v: %s = %q, want %q", req, name, h.Get(name), want)
			}
		}
	}
}

func TestTheInterfaceOnlyReads(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		for _, path := range []string{"/", "/api/v1/sessions", "/api/v1/refresh"} {
			w := do(t, request{method: method, path: path, cookie: testToken})
			if w.Code != http.StatusMethodNotAllowed || strings.Contains(w.Body.String(), "asked") {
				t.Errorf("%s %s: %d %s", method, path, w.Code, w.Body)
			}
		}
	}
	for path, want := range map[string]string{"/app.js": "javascript", "/app.css": "text/css"} {
		w := do(t, request{path: path})
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), want) {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, path := range []string{"/index.html", "/notice.html", "/static/app.js", "/../web.go", "/app.js/", "/favicon.ico"} {
		if w := do(t, request{path: path, cookie: testToken}); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}

func TestLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:4893", "127.0.0.1:0", "[::1]:4893", "localhost:4893", "127.0.0.2:1"} {
		if err := LoopbackAddr(addr); err != nil {
			t.Errorf("LoopbackAddr(%q) = %v", addr, err)
		}
	}
	for _, addr := range []string{":4893", "0.0.0.0:4893", "[::]:4893", "192.168.1.20:4893", "100.64.0.2:4893",
		"example.com:4893", "localhost.example.com:80", "4893", "127.0.0.1", ""} {
		if err := LoopbackAddr(addr); err == nil {
			t.Errorf("LoopbackAddr(%q) accepted an address that is not this machine only", addr)
		}
	}
}

func TestTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.token")
	if _, err := ReadToken(path); err == nil {
		t.Error("ReadToken made up a token")
	}
	first, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Errorf("token = %q, want 32 random bytes in hex", first)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("token file: %v, %v; want mode 600", st, err)
		}
	}
	again, err := LoadToken(path)
	if err != nil || again != first {
		t.Errorf("second LoadToken = %q, %v; want the same token", again, err)
	}
	if read, err := ReadToken(path); err != nil || read != first {
		t.Errorf("ReadToken = %q, %v", read, err)
	}
	other, err := LoadToken(filepath.Join(t.TempDir(), "web.token"))
	if err != nil || other == first {
		t.Errorf("another directory got the same token (%v)", err)
	}
	if got := URL("127.0.0.1:4893", first); got != "http://127.0.0.1:4893/?token="+first {
		t.Errorf("URL = %q", got)
	}
}

// What the page shows is not to be trusted, so the page may not have the
// means to run it: no markup built from strings in the script, and nothing
// inline in the documents, which the Content-Security-Policy would block
// anyway.
func TestPageHasNoWayToRunWhatItShows(t *testing.T) {
	script, err := static.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(script), "")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(",
		"new Function", "setAttribute(", "srcdoc", "javascript:", ".html("} {
		if strings.Contains(code, banned) {
			t.Errorf("app.js uses %q", banned)
		}
	}
	for _, name := range []string{"index.html", "notice.html"} {
		raw, err := static.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		page := string(raw)
		if regexp.MustCompile(`(?i)<script(\s[^>]*)?>[^<]`).MatchString(page) ||
			regexp.MustCompile(`(?i)<style|\sstyle=|\son[a-z]+=|javascript:`).MatchString(page) {
			t.Errorf("%s has inline script, style or an event handler", name)
		}
		if regexp.MustCompile(`(?i)(src|href)="(https?:)?//`).MatchString(page) {
			t.Errorf("%s loads something from another server", name)
		}
	}
}
