package relay

import (
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

// Admission gate (docs 6.6.2/6.6.3): the relay exposes a constrained "real Claude Code
// agent" surface, not a bare /v1/messages passthrough. Anything a coding agent cannot
// faithfully serve is rejected with a documented 4xx pointing at the official API.
//
// The payload checked here is the translated Anthropic /v1/messages body.
type Admission struct {
	cfg config.RelayAdmissionConfig
}

// NewAdmission builds the gate from config.
func NewAdmission(cfg config.RelayAdmissionConfig) *Admission {
	return &Admission{cfg: cfg}
}

// defaultCCToolset is the advertised Claude Code tool surface. Tenants may not declare
// custom schemas; when allow-tools is on, an exact-name subset of this set is tolerated.
var defaultCCToolset = map[string]struct{}{
	"Agent": {}, "Bash": {}, "BashOutput": {}, "Edit": {}, "ExitPlanMode": {},
	"Glob": {}, "Grep": {}, "KillShell": {}, "MultiEdit": {}, "NotebookEdit": {},
	"NotebookLaunch": {}, "Read": {}, "Task": {}, "TodoWrite": {}, "WebFetch": {},
	"WebSearch": {}, "Write": {},
}

// Admit inspects one inbound (translated) request body. A nil return admits the turn;
// otherwise a *RejectError carries the documented 4xx.
func (a *Admission) Admit(payload []byte) error {
	body := gjson.ParseBytes(payload)

	// Arbitrary system blocks: the agent's system stays the claude_code preset plus an
	// append-only persona (docs 1.2.1). Tenant-supplied system is unservable.
	if sys := body.Get("system"); sys.Exists() && !isEmptyJSONValue(sys) {
		return reject(400, "custom system prompts are unsupported on the coding-agent surface; use the official API")
	}

	// Tool schemas (docs 6.6.3): custom schemas are unservable; a tenant-declared
	// exact-name subset of the default CC toolset is tolerated.
	if tools := body.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		if !a.cfg.AllowDefaultToolset() {
			return reject(400, "custom tool schemas are unsupported on the coding-agent surface; use the official API")
		}
		if !isDefaultCCToolset(tools) {
			return reject(400, "custom tool schemas are unsupported on the coding-agent surface; use the official API")
		}
	}

	// Assistant prefill: a trailing assistant message primes the completion, which a real
	// CC conversation cannot faithfully reproduce (history comes from the CC loop).
	messages := body.Get("messages")
	if messages.Exists() && messages.IsArray() {
		arr := messages.Array()
		if n := len(arr); n > 0 {
			if strings.EqualFold(arr[n-1].Get("role").String(), "assistant") {
				return reject(400, "assistant prefill is unsupported on the coding-agent surface; use the official API")
			}
		}
	}

	// json-mode / stop-sequence semantics (docs 6.6.3).
	if rf := body.Get("response_format"); rf.Exists() && !isEmptyJSONValue(rf) {
		return reject(400, "json-mode semantics are unsupported on the coding-agent surface; use the official API")
	}
	if ss := body.Get("stop_sequences"); ss.Exists() && ss.IsArray() && len(ss.Array()) > 0 {
		return reject(400, "stop-sequence semantics are unsupported on the coding-agent surface; use the official API")
	}

	// Non-coding shape (lightweight classifier gate, docs S8/S15/H7).
	if a.codingGateEnabled() && !looksLikeCoding(messages) {
		return reject(400, "non-coding traffic is unsupported on the coding-agent surface; route to the official/enterprise API")
	}
	return nil
}

func (a *Admission) codingGateEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(a.cfg.CodingGate), "off")
}

func isEmptyJSONValue(v gjson.Result) bool {
	switch {
	case v.Type == gjson.Null:
		return true
	case v.Type == gjson.String:
		return strings.TrimSpace(v.String()) == ""
	case v.IsArray():
		return len(v.Array()) == 0
	case v.IsObject():
		return len(v.Map()) == 0
	}
	return false
}

func isDefaultCCToolset(tools gjson.Result) bool {
	ok := true
	tools.ForEach(func(_, tool gjson.Result) bool {
		name := tool.Get("name").String()
		if name == "" {
			// Server-defined tool types (e.g. web_search_20250305) are not part of the
			// advertised local toolset; treat as custom.
			ok = false
			return false
		}
		if _, hit := defaultCCToolset[name]; !hit {
			ok = false
			return false
		}
		return true
	})
	return ok
}

// codingSignals are substrings marking a turn as coding-shaped. Matching is
// case-insensitive over the concatenated user text.
var codingSignals = []string{
	"```", "def ", "class ", "function", "import ", "return ", "const ", "var ",
	"error", "panic", "exception", "stack trace", "traceback", "compile", "build",
	"debug", "refactor", "implement", "bug", "fix", "test", "unittest", "pytest",
	"git ", "commit", "branch", "merge", "npm", "pip ", "cargo", "docker",
	"api", "endpoint", "sql", "query", "schema", "json", "yaml", "regex",
	".go", ".py", ".js", ".ts", ".tsx", ".java", ".rs", ".c", ".cpp", ".h",
	".md", ".sh", ".sql", ".html", ".css", ".vue", ".jsx",
	"src/", "pkg/", "cmd/", "/home/", "~/", "d:\\", "c:\\",
	"代码", "函数", "报错", "编译", "调试", "重构", "实现", "接口", "变量", "文件",
	"脚本", "构建", "部署", "单元测试", "仓库", "分支", "提交", "合并", "修复",
}

// shortTurnLen bounds "continuation" turns ("继续", "yes, go on") that are natural inside
// an established coding session and must not be rejected by the classifier.
const shortTurnLen = 40

func looksLikeCoding(messages gjson.Result) bool {
	if !messages.Exists() || !messages.IsArray() {
		return true // no message content to classify; structural checks already ran
	}
	var b strings.Builder
	for _, msg := range messages.Array() {
		if !strings.EqualFold(msg.Get("role").String(), "user") {
			continue
		}
		extractText(msg.Get("content"), &b)
	}
	text := b.String()
	if strings.TrimSpace(text) == "" {
		// Tool results / image-only turns are coding-agent native shapes.
		return true
	}
	if utf8.RuneCountInString(strings.TrimSpace(text)) <= shortTurnLen {
		return true
	}
	lower := strings.ToLower(text)
	for _, sig := range codingSignals {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// extractText flattens user message content (string or content blocks) into text.
func extractText(content gjson.Result, b *strings.Builder) {
	if !content.Exists() {
		return
	}
	if content.Type == gjson.String {
		b.WriteString(content.String())
		b.WriteByte('\n')
		return
	}
	if content.IsArray() {
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				b.WriteString(block.Get("text").String())
				b.WriteByte('\n')
			}
		}
	}
}
