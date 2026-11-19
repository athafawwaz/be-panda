package handler

import (
	"strings"
	"testing"
)

// Dependency-free: go test internal/handler/version.go internal/handler/version_test.go
func TestValidVersionLabel(t *testing.T) {
	for _, value := range []string{"v1.0", "v2.1-beta", "Release 2026.01", "versi é", strings.Repeat("a", 50)} {
		if !validVersionLabel(value) {
			t.Errorf("valid version rejected: %q", value)
		}
	}
	for _, value := range []string{"", ".", "..", "../../escape", `..\..\escape`, "/tmp/escape", "C:escape", "v1\x00", "v1\n", `v1"`, "v1?", string([]byte{0xff}), strings.Repeat("a", 51)} {
		if validVersionLabel(value) {
			t.Errorf("unsafe version accepted: %q", value)
		}
	}
}
