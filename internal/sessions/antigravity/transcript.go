package antigravity

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Hebi8/hebi8XC/internal/sessions"
)

type transcriptStep struct {
	Index    int64  `json:"step_index"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Content  string `json:"content"`
	Thinking string `json:"thinking"`
	Calls    []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
	Input      *int64          `json:"input_tokens"`
	Output     *int64          `json:"output_tokens"`
	CacheRead  *int64          `json:"cache_read_tokens"`
	CacheWrite *int64          `json:"cache_write_tokens"`
	ExitCode   int             `json:"exit_code"`
	Error      json.RawMessage `json:"error"`
}

func transcriptPath(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(path)), "brain", strings.TrimSuffix(filepath.Base(path), ".db"), ".system_generated", "logs", "transcript.jsonl")
}

// The transcript carries no call ids on result steps. Within each planner
// step its results follow the calls in order; a new planner/user step ends
// that group. Message ids remain the database step indices. Structure is
// optional: a transcript that cannot be read leaves the database text, and
// false says so.
type transcriptMessage struct {
	sessions.Message
	tokens *sessions.Tokens
}

func transcript(path string) (map[int64]transcriptMessage, bool) {
	f, err := os.Open(transcriptPath(path))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := map[int64]transcriptMessage{}
	cwd := projectPath(path)
	reader := bufio.NewReader(f)
	var host int64
	next := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, false
		}
		if len(line) > 0 {
			var step transcriptStep
			e := json.Unmarshal(line, &step)
			// A line that is not JSON says nothing, so it is skipped. A field
			// of an unexpected type still leaves the line's type: that step is
			// dropped, but it keeps its place, or the results after it would
			// land on other calls.
			var mistyped *json.UnmarshalTypeError
			if e != nil && !errors.As(e, &mistyped) {
				if err == io.EOF {
					break
				}
				continue
			}
			switch step.Type {
			case "USER_INPUT":
				host = -1
			case "PLANNER_RESPONSE":
				if e != nil {
					host, next = -1, 0
					break
				}
				m := transcriptMessage{Message: sessions.Message{Kind: "text", Thinking: step.Thinking}}
				if step.Input != nil {
					m.tokens = &sessions.Tokens{Input: step.Input, Output: step.Output, CacheRead: step.CacheRead, CacheWrite: step.CacheWrite}
					m.Usage = &sessions.Usage{Input: *step.Input}
					if step.Output != nil {
						m.Usage.Output = *step.Output
					}
					if step.CacheRead != nil {
						m.Usage.CacheRead = *step.CacheRead
					}
					if step.CacheWrite != nil {
						m.Usage.CacheWrite = *step.CacheWrite
					}
				}
				for i, c := range step.Calls {
					input := string(c.Args)
					var str string
					if json.Unmarshal(c.Args, &str) == nil {
						input = str
					}
					m.Calls = append(m.Calls, sessions.Call{Call: fmt.Sprintf("step-%d-%d", step.Index, i), Tool: c.Name, Input: input, Error: step.Status == "ERROR", Files: toolFiles(c.Name, input, cwd)})
				}
				out[step.Index] = m
				host = step.Index
				next = 0
			case "VIEW_FILE", "RUN_COMMAND", "GREP_SEARCH", "LIST_DIRECTORY", "CODE_ACTION", "BROWSER_SUBAGENT", "SEARCH_WEB", "READ_URL_CONTENT", "ASK_QUESTION", "GENERIC", "ERROR_MESSAGE", "MCP_TOOL", "AGENCY_TOOL_CALL":
				if e != nil {
					next++
					break
				}
				m := out[host]
				if next < len(m.Calls) {
					c := &m.Calls[next]
					c.Output = step.Content
					c.Error = c.Error || step.Status == "ERROR" || step.Type == "ERROR_MESSAGE" || step.ExitCode != 0 || (len(step.Error) > 0 && string(step.Error) != "null" && string(step.Error) != `""`)
					if c.Output == "" && c.Error {
						var text string
						if json.Unmarshal(step.Error, &text) == nil {
							c.Output = text
						}
					}
					out[host] = m
					next++
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	return out, true
}

// Only explicit file tools observed in ACP transcripts are mapped. Shell
// commands and MCP calls remain opaque; their text is not a file manifest.
func toolFiles(tool, input, cwd string) []sessions.File {
	var fields []string
	op := "change"
	switch tool {
	case "client_create_file", "client_edit_file":
		fields = []string{"target_file"}
	case "write_to_file", "replace_file_content":
		fields = []string{"TargetFile"}
	case "client_view_file":
		fields, op = []string{"absolute_path"}, "read"
	case "view_file":
		fields, op = []string{"AbsolutePath", "absolute_path"}, "read"
	default:
		return nil
	}
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &args) != nil {
		return nil
	}
	for _, field := range fields {
		var path string
		if json.Unmarshal(args[field], &path) != nil || strings.TrimSpace(path) == "" {
			continue
		}
		var decoded string
		if json.Unmarshal([]byte(path), &decoded) == nil {
			path = decoded
		}
		if !filepath.IsAbs(path) && filepath.IsAbs(cwd) {
			path = filepath.Join(cwd, path)
		}
		return []sessions.File{{Path: path, Op: op}}
	}
	return nil
}
