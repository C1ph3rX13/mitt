package httpwire

import "strings"

// Values returns every value for name in header order. Header names match
// case-insensitively; duplicate names are preserved.
func Values(headers [][2]string, name string) []string {
	values := []string{}
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			values = append(values, header[1])
		}
	}
	return values
}

// Has reports whether name appears at least once.
func Has(headers [][2]string, name string) bool {
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			return true
		}
	}
	return false
}

// First returns the first value for name, or an empty string when absent.
func First(headers [][2]string, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header[0], name) {
			return header[1]
		}
	}
	return ""
}

// Strip returns a copy of headers without any field named name. The input
// slice is never modified; order and duplicates of the remaining fields are
// preserved.
func Strip(headers [][2]string, name string) [][2]string {
	stripped := make([][2]string, 0, len(headers))
	for _, header := range headers {
		if !strings.EqualFold(header[0], name) {
			stripped = append(stripped, header)
		}
	}
	return stripped
}

// ValidToken reports whether value is an RFC 9110 token. It is the shared
// rule for method and header names.
func ValidToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			continue
		case strings.IndexByte("!#$%&'*+-.^_`|~", character) >= 0:
			continue
		default:
			return false
		}
	}
	return true
}

// ValidHeaderValue reports whether value contains no control characters other
// than horizontal tab. obs-text (0x80-0xff) is accepted.
func ValidHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func clonePairs(pairs [][2]string) [][2]string {
	cloned := make([][2]string, len(pairs))
	copy(cloned, pairs)
	return cloned
}
