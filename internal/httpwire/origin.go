package httpwire

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Origin is where one request is addressed: the origin's scheme and authority
// plus the origin-form request-target. It is the one place that renders the
// two forms the rest of the program needs from those parts:
//
//   - URL is the absolute form history stores, and
//   - WireTarget is the origin-form target written to the next hop.
//
// Keeping both in one value means a recorded URL cannot disagree with the
// request-target that was actually sent.
type Origin struct {
	Scheme    string // http or https, lowercased
	Userinfo  string // userinfo as written, without the @; never sent as an authority
	Authority string // host[:port] as addressed, without userinfo
	Target    string // origin-form request-target; "" means no path, "*" is asterisk-form
}

// NewOrigin validates the three parts and returns the origin they describe.
// Target must be origin-form ("/...", "*", or empty); absolute-form is the
// caller's business to split first.
func NewOrigin(scheme, authority, target string) (Origin, error) {
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return Origin{}, fmt.Errorf("origin scheme %q is not http or https", scheme)
	}
	if authority == "" {
		return Origin{}, errors.New("origin authority is empty")
	}
	if strings.ContainsAny(authority, "/?#") || !ValidHeaderValue(authority) {
		return Origin{}, fmt.Errorf("invalid origin authority %q", authority)
	}
	if parsed, err := url.Parse(scheme + "://" + authority); err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return Origin{}, fmt.Errorf("invalid origin authority %q", authority)
	}
	switch {
	case target == "" || target == "*":
	case strings.HasPrefix(target, "/"):
		if strings.ContainsAny(target, " \t\r\n") {
			return Origin{}, fmt.Errorf("invalid request target %q", target)
		}
	default:
		return Origin{}, fmt.Errorf("request target %q is not origin-form", target)
	}
	return Origin{Scheme: scheme, Authority: authority, Target: target}, nil
}

// OriginOfURL describes the origin addressed by an absolute URL. The target
// keeps the URL's escaped path and query exactly; an absent path stays
// absent, so rendering the URL back gives the same string.
func OriginOfURL(u *url.URL) Origin {
	origin := Origin{Scheme: strings.ToLower(u.Scheme), Authority: u.Host}
	if u.User != nil {
		origin.Userinfo = u.User.String()
	}
	origin.Target = escapedTarget(u)
	return origin
}

// escapedTarget is the origin-form request-target of u, without the "/"
// normalization RequestURI applies: a URL with no path yields "".
func escapedTarget(u *url.URL) string {
	if u.Opaque != "" {
		return u.Opaque
	}
	target := u.EscapedPath()
	switch {
	case u.RawQuery != "":
		target += "?" + u.RawQuery
	case u.ForceQuery:
		target += "?"
	}
	return target
}

// URL is the absolute URL of the request. An asterisk-form target addresses
// the origin itself and contributes no path.
func (o Origin) URL() string {
	prefix := o.Scheme + "://"
	if o.Userinfo != "" {
		prefix += o.Userinfo + "@"
	}
	prefix += o.Authority
	if o.Target == "" || o.Target == "*" {
		return prefix
	}
	return prefix + o.Target
}

// WireTarget is the request-target written on the wire. An empty target and a
// bare query both need the "/" that only exists once the request is rendered.
func (o Origin) WireTarget() string {
	switch {
	case o.Target == "":
		return "/"
	case o.Target == "*" || strings.HasPrefix(o.Target, "/"):
		return o.Target
	default:
		return "/" + o.Target
	}
}
