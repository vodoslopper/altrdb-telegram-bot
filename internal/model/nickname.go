package model

import (
	"fmt"
	"regexp"
	"strings"
)

// nicknameRe is the shape of an ALT maintainer nickname (the part before "@"
// in a packager email). 1..64 characters, starting alphanumeric.
var nicknameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._+\-]{0,63}$`)

// nicknameSplitRe covers everything a user is likely to paste: commas,
// semicolons, and any run of whitespace including newlines.
var nicknameSplitRe = regexp.MustCompile(`[\s,;]+`)

// MaxNicknamesPerChat bounds how many maintainers one chat may follow. It
// exists to keep the fan-out join and the settings screen sane.
const MaxNicknamesPerChat = 64

// ErrInvalidNickname is returned for a token that cannot be a nickname.
var ErrInvalidNickname = fmt.Errorf("invalid nickname")

// NormalizeNickname trims, drops a leading "@", lower-cases and validates one
// token. It returns ErrInvalidNickname (wrapped, with the offending token)
// when the result does not match the nickname grammar.
func NormalizeNickname(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "@")
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidNickname)
	}
	if !nicknameRe.MatchString(s) {
		return "", fmt.Errorf("%w: %q", ErrInvalidNickname, s)
	}
	return s, nil
}

// ParseNicknames splits free-form user input into validated, de-duplicated
// nicknames. Tokens that fail validation are returned separately, in input
// order and also de-duplicated, so the caller can tell the user exactly what
// was rejected instead of silently dropping it.
func ParseNicknames(input string) (valid []string, invalid []string) {
	seenValid := map[string]struct{}{}
	seenInvalid := map[string]struct{}{}

	for _, token := range nicknameSplitRe.Split(input, -1) {
		if strings.TrimSpace(token) == "" {
			continue
		}
		norm, err := NormalizeNickname(token)
		if err != nil {
			key := strings.ToLower(strings.TrimSpace(token))
			if _, dup := seenInvalid[key]; !dup {
				seenInvalid[key] = struct{}{}
				invalid = append(invalid, strings.TrimSpace(token))
			}
			continue
		}
		if _, dup := seenValid[norm]; dup {
			continue
		}
		seenValid[norm] = struct{}{}
		valid = append(valid, norm)
	}
	return valid, invalid
}

// ValidNickname reports whether s is already a normalised nickname.
func ValidNickname(s string) bool { return nicknameRe.MatchString(s) }
