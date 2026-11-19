package storage

import (
	"fmt"
	"io/fs"
	"strings"
	"unicode"
)

func validateKey(key string) error {
	if key == "." || !fs.ValidPath(key) || strings.ContainsAny(key, `\:<>"|?*`) || strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return fmt.Errorf("key storage harus berupa path relatif tanpa traversal: %q", key)
	}
	return nil
}
