package handler

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

func validVersionLabel(version string) bool {
	return version != "" && version != "." && version != ".." &&
		utf8.ValidString(version) && utf8.RuneCountInString(version) <= 50 &&
		!strings.ContainsAny(version, `/\:<>"|?*`) &&
		strings.IndexFunc(version, unicode.IsControl) < 0
}
