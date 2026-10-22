package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// Runs without third-party dependencies: go test cmd/api/authorization_test.go -v
func backendPath(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

func TestStartupValidatesSecret(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), backendPath("cmd", "api", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	validated := false
	ast.Inspect(file, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if function, ok := call.Fun.(*ast.SelectorExpr); ok && function.Sel.Name == "ValidateAppSecret" {
				validated = true
			}
		}
		return true
	})
	if !validated {
		t.Fatal("API startup must validate JWT_SECRET")
	}
}

func TestAuthorizationGuardsRemainWired(t *testing.T) {
	for _, tc := range []struct {
		file, function string
		guards         []string
	}{
		{"internal/handler/handler.go", "AuthMiddleware", []string{"WithValidMethods", "WithExpirationRequired"}},
		{"internal/handler/handler.go", "GetApplicationDetail", []string{"getCurrentUser", "canAccessFolder"}},
		{"internal/handler/handler.go", "CreateAppSection", []string{"getCurrentUser", "canAccessFolder"}},
		{"internal/handler/handler.go", "serveDocumentFile", []string{"getCurrentUser", "ApplyDocumentScope"}},
		{"internal/handler/handler.go", "GetEvidenceList", []string{"ApplyDocumentScope"}},
		{"internal/handler/handler.go", "GetDashboardSummary", []string{"ApplyDocumentScope", "applyFolderScope", "scopedUploadLogs"}},
		{"internal/handler/handler.go", "GetUploadLogs", []string{"scopedUploadLogs"}},
		{"internal/service/ai_agent.go", "scopedDocuments", []string{"ApplyDocumentScope"}},
		{"internal/handler/handler.go", "CreateApplication", []string{"validVersionLabel"}},
		{"internal/handler/handler.go", "CreateAppSection", []string{"validVersionLabel"}},
		{"internal/handler/upload_common.go", "normalizeDocumentMeta", []string{"validVersionLabel"}},
		{"internal/storage/storage.go", "Save", []string{"validateKey"}},
		{"internal/storage/storage.go", "Exists", []string{"validateKey"}},
	} {
		t.Run(tc.function, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), backendPath(tc.file), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]bool{}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != tc.function {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						switch fun := call.Fun.(type) {
						case *ast.Ident:
							calls[fun.Name] = true
						case *ast.SelectorExpr:
							calls[fun.Sel.Name] = true
						}
					}
					return true
				})
			}
			for _, guard := range tc.guards {
				if !calls[guard] {
					t.Errorf("%s must call %s", tc.function, guard)
				}
			}
		})
	}
}
