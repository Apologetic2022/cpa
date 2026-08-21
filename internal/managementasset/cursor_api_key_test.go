package managementasset

import (
	"os"
	"strings"
	"testing"
)

// fixtureManagementHTML mirrors the anchor shapes found in the minified
// upstream Management Center bundle that the injection regexes rely on.
const fixtureManagementHTML = "<!doctype html><html><head></head><body><script>" +
	"const nav=[{path:`/oauth`,labelKey:`nav.oauth`,metaKey:`nav_meta.oauth`,icon:(0,x.jsx)(Ic,{})}," +
	"{path:`/quota`,labelKey:`nav.quota_management`,metaKey:`nav_meta.quota_management`,icon:(0,x.jsx)(Qc,{})}];" +
	"const routes=[{path:`/oauth`,element:(0,rt.jsx)(Om,{})},{path:`/quota`,element:(0,rt.jsx)(Qm,{})}];" +
	"const zh={nav:{oauth:`OAuth 授权`,quota_management:`配额管理`},nav_meta:{oauth:`第三方授权`,quota_management:`用量控制`}};" +
	"const en={nav:{oauth:`OAuth`,quota_management:`Quota`},nav_meta:{oauth:`OAuth providers`,quota_management:`Usage control`}};" +
	"</script></body></html>"

func TestAddCursorAPIKeyManagerToManagementHTML(t *testing.T) {
	out, err := AddCursorAPIKeyManagerToManagementHTML(fixtureManagementHTML)
	if err != nil {
		t.Fatalf("AddCursorAPIKeyManagerToManagementHTML() error = %v", err)
	}

	if !strings.Contains(out, "{path:`/cursor-api-key`,labelKey:`nav.cursor_api_key`,metaKey:`nav_meta.cursor_api_key`,icon:(0,x.jsx)(Ic,{})}") {
		t.Fatal("sidebar nav entry not injected with reused OAuth icon expression")
	}
	if !strings.Contains(out, "{path:`/cursor-api-key`,element:(0,rt.jsx)(\"div\",{id:\"cpa-native-route-host\"})}") {
		t.Fatal("SPA route not injected with captured jsx module alias")
	}
	if got := strings.Count(out, "cursor_api_key:`Cursor API Key`"); got != 4 {
		t.Fatalf("locale labels injected %d times, want 4 (nav+nav_meta for two locales)", got)
	}
	if !strings.Contains(out, cursorAPIKeyManagerScriptID) {
		t.Fatal("widget script not injected before </body>")
	}
	if idx := strings.Index(out, "<script id=\""+cursorAPIKeyManagerScriptID+"\">"); idx < 0 || idx > strings.LastIndex(out, "</body>") {
		t.Fatal("widget script must be injected inside <body>")
	}
}

func TestAddCursorAPIKeyManagerToManagementHTMLIdempotent(t *testing.T) {
	once, err := AddCursorAPIKeyManagerToManagementHTML(fixtureManagementHTML)
	if err != nil {
		t.Fatalf("first injection error = %v", err)
	}
	twice, err := AddCursorAPIKeyManagerToManagementHTML(once)
	if err != nil {
		t.Fatalf("second injection error = %v", err)
	}
	if twice != once {
		t.Fatal("second injection must be a no-op")
	}
}

func TestAddCursorAPIKeyManagerToManagementHTMLMissingAnchor(t *testing.T) {
	const html = "<!doctype html><html><body><script>const nav=[];</script></body></html>"
	out, err := AddCursorAPIKeyManagerToManagementHTML(html)
	if err == nil {
		t.Fatal("expected error when nav anchor is missing")
	}
	if out != html {
		t.Fatal("input must be returned unmodified when injection fails")
	}
}

// TestAddCursorAPIKeyManagerToRealAsset runs against a real Management Center
// bundle when CPA_MANAGEMENT_HTML_FIXTURE points at one (e.g. a copy of the
// production static/management.html). Skipped otherwise.
func TestAddCursorAPIKeyManagerToRealAsset(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("CPA_MANAGEMENT_HTML_FIXTURE"))
	if path == "" {
		t.Skip("CPA_MANAGEMENT_HTML_FIXTURE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := AddCursorAPIKeyManagerToManagementHTML(string(data))
	if err != nil {
		t.Fatalf("injection against real asset failed: %v", err)
	}
	for _, want := range []string{
		"labelKey:`nav.cursor_api_key`",
		"path:`/cursor-api-key`,element:",
		cursorAPIKeyManagerScriptID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("real asset injection missing %q", want)
		}
	}
	if got := strings.Count(out, "cursor_api_key:`Cursor API Key`"); got < 2 {
		t.Fatalf("locale labels injected %d times, want at least nav+nav_meta", got)
	}
	again, err := AddCursorAPIKeyManagerToManagementHTML(out)
	if err != nil || again != out {
		t.Fatalf("second injection must be a no-op (err=%v, changed=%t)", err, again != out)
	}
}
