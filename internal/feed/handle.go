package feed

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const MaxHandleRunes = 64

var ErrBadHandle = errors.New("handle must be 1-64 letters, marks, digits, '.', '_' or '-'")

// NormalizeHandle returns the handle as shown (NFKC) and the key it is
// compared and stored under (case-folded NFKC).
func NormalizeHandle(in string) (display, key string, err error) {
	if !utf8.ValidString(in) {
		return "", "", ErrBadHandle
	}
	display = norm.NFKC.String(strings.TrimSpace(in))
	if !validHandle(display) {
		return "", "", ErrBadHandle
	}
	key = norm.NFKC.String(cases.Fold().String(display))
	if !validHandle(key) {
		return "", "", ErrBadHandle
	}
	return display, key, nil
}

func validHandle(s string) bool {
	n := 0
	for _, r := range s {
		n++
		if n > MaxHandleRunes {
			return false
		}
		if !unicode.IsLetter(r) && !unicode.IsMark(r) && !unicode.IsDigit(r) && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return n > 0
}
