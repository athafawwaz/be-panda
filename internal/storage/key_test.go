package storage

import "testing"

// Dependency-free: go test internal/storage/key.go internal/storage/key_test.go
func TestValidateKey(t *testing.T) {
	for _, key := range []string{"STD_SOP_v1.0.pdf", "_staging/abc.pdf", "versi é.pdf"} {
		if err := validateKey(key); err != nil {
			t.Errorf("valid key %q: %v", key, err)
		}
	}
	for _, key := range []string{"", ".", "..", "../escape", "APP_SOP_x/../../../escape.pdf", "a/../b", "/absolute", `C:\outside.pdf`, `a\..\b`, "a//b", "a/", "a\x00b", "a\nb", "a:b", string([]byte{0xff})} {
		if err := validateKey(key); err == nil {
			t.Errorf("unsafe key accepted: %q", key)
		}
	}
}
