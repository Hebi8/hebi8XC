package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// chrome is a headless Chrome with one page, driven over the pipe of its
// DevTools protocol: JSON messages that end in a zero byte, written to its
// file 3 and read from its file 4. That needs no library and no port.
type chrome struct {
	cmd *exec.Cmd
	dir string
	to  *os.File

	mu      sync.Mutex
	last    int
	waiting map[int]chan reply
	session string   // of the page
	agent   string   // what the browser calls itself
	trouble []string // dialogs that opened and exceptions that were not caught
}

type reply struct {
	Result json.RawMessage
	Error  *struct{ Message string }
}

// findChrome returns the browser to run: the one asked for, or the first
// of the usual names.
func findChrome(asked string) (string, error) {
	names := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	if asked != "" {
		names = []string{asked}
	}
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no Chrome found; name one with -chrome")
}

func startChrome(bin string) (*chrome, error) {
	dir, err := os.MkdirTemp("", "h8c-websmoke-chrome-")
	if err != nil {
		return nil, err
	}
	toR, toW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromR, fromW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c := &chrome{dir: dir, to: toW, waiting: map[int]chan reply{}}
	// --password-store=basic: without it Chrome asks the keyring of the
	// desktop for the key of its cookie store, and where that does not
	// answer every page that sets a cookie hangs.
	c.cmd = exec.Command(bin, "--headless=new", "--remote-debugging-pipe", "--password-store=basic",
		"--user-data-dir="+dir, "--no-first-run", "--no-default-browser-check", "--disable-gpu",
		"--disable-background-networking", "--disable-component-update", "--disable-sync",
		"--window-size=1280,900", "--lang=en-US")
	c.cmd.ExtraFiles = []*os.File{toR, fromW}
	if err := c.cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	toR.Close()
	fromW.Close()
	go c.read(fromR)

	var target struct{ TargetID string }
	if err := c.call("Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		c.close()
		return nil, err
	}
	var attached struct{ SessionID string }
	if err := c.call("Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		c.close()
		return nil, err
	}
	c.session = attached.SessionID
	for _, domain := range []string{"Page.enable", "Runtime.enable"} {
		if err := c.call(domain, nil, nil); err != nil {
			c.close()
			return nil, err
		}
	}
	return c, nil
}

// read takes what Chrome says: answers go to who asked, and of the events
// the two are kept that mean that something ran which should not have.
func (c *chrome) read(from *os.File) {
	r := bufio.NewReaderSize(from, 1<<20)
	for {
		raw, err := r.ReadBytes(0)
		if err != nil {
			c.mu.Lock()
			for id, ch := range c.waiting {
				ch <- reply{Error: &struct{ Message string }{"Chrome is gone"}}
				delete(c.waiting, id)
			}
			c.mu.Unlock()
			return
		}
		var m struct {
			ID     int
			Method string
			Params json.RawMessage
			reply
		}
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil {
			continue
		}
		c.mu.Lock()
		switch {
		case m.Method == "Page.javascriptDialogOpening":
			c.trouble = append(c.trouble, "a dialog opened: "+string(m.Params))
			go c.send(c.session, "Page.handleJavaScriptDialog", map[string]any{"accept": true}, nil)
		case m.Method == "Runtime.exceptionThrown":
			c.trouble = append(c.trouble, "an exception: "+string(m.Params))
		case m.Method == "" && c.waiting[m.ID] != nil:
			c.waiting[m.ID] <- m.reply
			delete(c.waiting, m.ID)
		}
		c.mu.Unlock()
	}
}

// call sends a command to the page, or before there is one to the browser,
// and waits for its answer.
func (c *chrome) call(method string, params, out any) error {
	return c.send(c.session, method, params, out)
}

func (c *chrome) send(session, method string, params, out any) error {
	c.mu.Lock()
	c.last++
	msg := map[string]any{"id": c.last, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	ch := make(chan reply, 1)
	c.waiting[c.last] = ch
	raw, _ := json.Marshal(msg)
	_, err := c.to.Write(append(raw, 0))
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s: no answer from Chrome in 30 s", method)
	}
}

// eval runs an expression in the page and puts its value, which is awaited
// if it is a promise, into out.
func (c *chrome) eval(expr string, out any) error {
	var r struct {
		Result           struct{ Value json.RawMessage }
		ExceptionDetails *struct {
			Text      string
			Exception struct{ Description string }
		}
	}
	err := c.call("Runtime.evaluate", map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true}, &r)
	if err != nil {
		return err
	}
	if r.ExceptionDetails != nil {
		return fmt.Errorf("%s: %s %s", expr, r.ExceptionDetails.Text, r.ExceptionDetails.Exception.Description)
	}
	if out == nil || r.Result.Value == nil {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

// view sets the width of the window and the language the browser asks
// for, which is also the language the page finds in it.
func (c *chrome) view(width int, language string) error {
	if c.agent == "" {
		var v struct{ UserAgent string }
		if err := c.send("", "Browser.getVersion", nil, &v); err != nil {
			return err
		}
		c.agent = v.UserAgent
	}
	if err := c.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": 844,
		"deviceScaleFactor": 1, "mobile": false}, nil); err != nil {
		return err
	}
	return c.call("Emulation.setUserAgentOverride", map[string]any{"userAgent": c.agent, "acceptLanguage": language}, nil)
}

// shot writes a picture of the page to a file: of what is in the window,
// or with whole of everything the page has, scrolled to or not.
func (c *chrome) shot(path string, whole bool) error {
	params := map[string]any{"format": "png"}
	if whole {
		var m struct {
			CSSContentSize struct{ Width, Height float64 }
		}
		if err := c.call("Page.getLayoutMetrics", nil, &m); err != nil {
			return err
		}
		params["captureBeyondViewport"] = true
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": m.CSSContentSize.Width, "height": m.CSSContentSize.Height, "scale": 1}
	}
	var r struct{ Data []byte }
	if err := c.call("Page.captureScreenshot", params, &r); err != nil {
		return err
	}
	return os.WriteFile(path, r.Data, 0o600)
}

// await waits until a condition holds in the page.
func (c *chrome) await(cond string) error {
	var err error
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var ok bool
		// An error is the page being between two documents, mostly.
		if err = c.eval("Boolean("+cond+")", &ok); err == nil && ok {
			return nil
		}
	}
	return fmt.Errorf("waited 15 s for %s (%v)", cond, err)
}

func (c *chrome) troubles() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.trouble...)
}

func (c *chrome) close() {
	done := make(chan struct{})
	go func() {
		c.send("", "Browser.close", nil, nil)
		c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		c.cmd.Process.Kill()
		<-done
	}
	c.to.Close()
	os.RemoveAll(c.dir)
}
