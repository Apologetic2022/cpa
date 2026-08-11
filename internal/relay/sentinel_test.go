package relay

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Link/dependency sentinel (docs 6.6.2): depguard is the compile-time gate; this test is
// the second tripwire. It asserts at the dependency-closure level that the relay invoke
// path can never reach an egress-capable stack:
//
//   - github.com/refraction-networking/utls must NOT appear anywhere in the closure
//     (deleted from this fork — a Go/uTLS JA4 on the Anthropic wire is fatal).
//   - crypto/tls must be reachable ONLY through the stdlib net/http package itself
//     (the two whitelisted socket files dial plaintext UDS/loopback; net/http's TLS
//     code is present but inert). No module or third-party package in the closure may
//     import crypto/tls — the invoke path never initiates TLS.
//   - net/http may be imported only by whitelisted files (socket clients, test stubs,
//     and executor.go whose usage is interface types ONLY — enforced by an AST scan
//     below, so no future edit can construct an HTTP client outside the socket files).
func TestDependencyClosure_NoEgressStacks(t *testing.T) {
	graph := depGraph(t, "github.com/router-for-me/CLIProxyAPI/v7/internal/relay")
	for pkg := range graph {
		if pkg == "github.com/refraction-networking/utls" || strings.HasPrefix(pkg, "github.com/refraction-networking/utls/") {
			t.Fatalf("FATAL: utls reachable from relay invoke closure (dep %s) — deleted from fork, docs 6.6.2", pkg)
		}
		if strings.HasPrefix(pkg, modulePath+"/internal/auth/") {
			t.Fatalf("FATAL: provider egress transport %s reachable from relay invoke closure — provider transports are deleted from the invoke path (docs 6.6.1)", pkg)
		}
		if pkg == modulePath+"/internal/runtime/executor/helps" {
			t.Fatalf("FATAL: helps reachable from relay invoke closure — it drags utls_client.go (docs 6.6.2); use the relay-local usage reporter")
		}
	}
	// crypto/tls in the closure is permitted ONLY for the management plane that docs
	// 6.6.1 explicitly retains (management API + WebUI): proxyutil builds proxy
	// transports for login/management flows, internal/home generates the WebUI cert.
	// The invoke path itself (internal/relay) never imports crypto/tls — enforced at
	// source level below. Any NEW module package importing crypto/tls here means an
	// egress-capable stack crept into the invoke closure: fail the build.
	allowedTLSImporters := map[string]bool{
		modulePath + "/sdk/proxyutil":  true,
		modulePath + "/internal/home":  true,
	}
	for pkg, imports := range graph {
		if !strings.HasPrefix(pkg, modulePath) {
			continue
		}
		for _, imp := range imports {
			if imp == "crypto/tls" && !allowedTLSImporters[pkg] {
				t.Fatalf("FATAL: %s imports crypto/tls in the relay invoke closure — module code on the invoke path never initiates TLS (docs 6.6.2)", pkg)
			}
		}
	}
}

const modulePath = "github.com/router-for-me/CLIProxyAPI/v7"

// Per-file direct import scan: only whitelisted socket/stub files (and executor.go,
// types-only) may import net/http; no relay source file (tests included) may import
// crypto/tls or utls.
func TestSourceImports_OnlyWhitelistedSockets(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("cannot enumerate relay sources: %v", err)
	}
	httpWhitelist := map[string]bool{
		"agentclient.go":       true, // per-account agent socket client
		"store_rqlite.go":      true, // host-local rqlited client
		"store_rqlite_test.go": true, // fake rqlited (httptest)
		"relay_test.go":        true, // in-memory agent stub (httptest)
		"executor.go":          true, // interface TYPES only — AST-checked below
	}
	fset := token.NewFileSet()
	for _, f := range files {
		base := filepath.Base(f)
		parsed, err := parser.ParseFile(fset, base, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", base, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", base, err)
			}
			switch {
			case path == "crypto/tls":
				t.Fatalf("FATAL: %s imports crypto/tls — invoke path never initiates TLS (docs 6.6.2)", base)
			case path == "github.com/refraction-networking/utls" || strings.HasPrefix(path, "github.com/refraction-networking/utls/"):
				t.Fatalf("FATAL: %s imports utls — deleted from fork (docs 6.6.2)", base)
			case path == "net/http" && !httpWhitelist[base]:
				t.Fatalf("FATAL: %s imports net/http outside the socket whitelist (docs 6.6.2: egress stacks must stay unreachable from the invoke path)", base)
			}
		}
	}
}

// AST symbol check: executor.go may reference net/http TYPES only (the provider
// executor interface forces *http.Request / *http.Response / http.Header into the
// signatures). Any client/transport/server constructor or package-level call is a
// smuggled egress path and fails the build.
func TestExecutorHTTPUsage_TypesOnly(t *testing.T) {
	allowed := map[string]bool{
		"Header":   true,
		"Request":  true,
		"Response": true,
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "executor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "http" {
			return true
		}
		if !allowed[sel.Sel.Name] {
			t.Fatalf("FATAL: executor.go uses http.%s — only type references (Header/Request/Response) are allowed on the invoke path; no client/transport/server construction (docs 6.6.2)", sel.Sel.Name)
		}
		return true
	})
}

func depGraph(t *testing.T, pkg string) map[string][]string {
	t.Helper()
	goBin := "go"
	if runtime.GOOS == "windows" {
		goBin = "go.exe"
	}
	out, err := exec.Command(goBin, "list", "-f", "{{.ImportPath}}|{{.Imports}}", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s failed: %v", pkg, err)
	}
	graph := make(map[string][]string)
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, "|")
		if sep < 0 {
			continue
		}
		path := strings.TrimSpace(line[:sep])
		imports := strings.TrimSpace(line[sep+1:])
		imports = strings.TrimPrefix(imports, "[")
		imports = strings.TrimSuffix(imports, "]")
		var list []string
		if imports != "" {
			list = strings.Fields(imports)
		}
		graph[path] = list
	}
	return graph
}
