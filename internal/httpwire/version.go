package httpwire

import "strings"

// Version is one HTTP version as it appears in two places: the token in a
// request or status line, and the token history stores in `http_version`.
// The two differ only for HTTP/2, which has no line on the wire and uses the
// short "h2" token in history, so one cannot be derived from the other by
// lowercasing.
type Version struct {
	// Line is the version token written in a request or status line.
	Line string
	// Token is the lowercase token persisted as `http_version`.
	Token string
}

var (
	// HTTP11 is HTTP/1.1 on both a line and in history.
	HTTP11 = Version{Line: "HTTP/1.1", Token: "http/1.1"}
	// HTTP2 is HTTP/2: its line form exists only as the head history can
	// render, because an HTTP/2 message carries no start line.
	HTTP2 = Version{Line: "HTTP/2.0", Token: "h2"}
)

// VersionOf returns the version named by a line token such as "HTTP/1.0" or
// "HTTP/1.1". Unknown versions keep their lowercased token, matching how
// history has always recorded them.
func VersionOf(version string) Version {
	switch version {
	case "HTTP/2.0", "HTTP/2":
		return HTTP2
	default:
		return Version{Line: version, Token: strings.ToLower(version)}
	}
}
