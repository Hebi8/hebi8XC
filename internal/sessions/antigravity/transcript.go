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
	Input     *int64          `json:"input_tokens"`
	Output    int64           `json:"output_tokens"`
	CacheRead int64           `json:"cache_read_tokens"`
	ExitCode  int             `json:"exit_code"`
	Error     json.RawMessage `json:"error"`
}

func transcriptPath(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(path)), "brain", strings.TrimSuffix(filepath.Base(path), ".db"), ".system_generated", "logs", "transcript.jsonl")
}

// The transcript carries no call ids on result steps. Within each planner
// step its results follow the calls in order; a new planner/user step ends
// that group. Message ids remain the database step indices.
func transcript(path string) (map[int64]sessions.Message, bool, error) {
	f, err := os.Open(transcriptPath(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	out := map[int64]sessions.Message{}
	reader := bufio.NewReader(f)
	var host int64
	next := 0
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		if len(line) > 0 {
			var step transcriptStep
			if e := json.Unmarshal(line, &step); e != nil {
				// Structure is optional; a damaged or unfinished line must
				// not hide the database text or the other transcript steps.
				if err == io.EOF {
					break
				}
				continue
			}
			switch step.Type {
			case "USER_INPUT":
				host = -1
			case "PLANNER_RESPONSE":
				m := sessions.Message{Kind: "text", Thinking: step.Thinking}
				if step.Input != nil {
					m.Usage = &sessions.Usage{Input: *step.Input, Output: step.Output, CacheRead: step.CacheRead}
				}
				for i, c := range step.Calls {
					input := string(c.Args)
					var str string
					if json.Unmarshal(c.Args, &str) == nil {
						input = str
					}
					m.Calls = append(m.Calls, sessions.Call{Call: fmt.Sprintf("step-%d-%d", step.Index, i), Tool: c.Name, Input: input, Error: step.Status == "ERROR"})
				}
				out[step.Index] = m
				host = step.Index
				next = 0
			case "VIEW_FILE", "RUN_COMMAND", "GREP_SEARCH", "LIST_DIRECTORY", "CODE_ACTION", "BROWSER_SUBAGENT", "SEARCH_WEB", "READ_URL_CONTENT", "ASK_QUESTION", "GENERIC", "ERROR_MESSAGE", "MCP_TOOL", "AGENCY_TOOL_CALL":
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
	return out, true, nil
}
