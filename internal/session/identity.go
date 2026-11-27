package session

import "github.com/C1ph3rX13/mitt/internal/httpwire"

// RequestIdentity is the identity of the request half of one exchange: the
// absolute URL it addresses, the line the client wrote (synthesized for
// protocols that have none), the line written to the next hop, and the
// protocol token. Callers derive it with MessageIdentity or StreamIdentity and
// apply it with Exchange.IdentifyRequest; they never write these fields
// themselves, so the three capture paths cannot disagree about the same
// request.
type RequestIdentity struct {
	Method               string
	URL                  string
	HTTPVersion          string
	RequestLine          string
	ForwardedRequestLine string
	// UpstreamCertSHA256 is the upstream origin's identity rather than the
	// request's. It is copied from the tunnel that served the exchange.
	UpstreamCertSHA256 string
}

// MessageIdentity derives the identity of an HTTP/1 request. head is the
// message whose line is recorded, and forwardedLine is the start line the next
// hop receives. They are the same for `send`, which writes the head it built;
// they differ for a proxy client, whose absolute-form line becomes origin-form.
//
// Taking the forwarded line from the head that is actually written means a
// rewritten target, a bridge between protocols, or a handler that forgets to
// update a field cannot leave the record claiming a line the next hop never
// received.
func MessageIdentity(head *httpwire.Request, origin httpwire.Origin, forwardedLine string) RequestIdentity {
	version := httpwire.VersionOf(head.Version)
	return RequestIdentity{
		Method:               head.Method,
		URL:                  origin.URL(),
		HTTPVersion:          version.Token,
		RequestLine:          head.StartLine,
		ForwardedRequestLine: forwardedLine,
	}
}

// StreamIdentity derives the identity of an HTTP/2 request, which carries no
// request line. The recorded line is synthesized so `show --raw` can render
// the exchange; the forwarded line names the protocol the next hop actually
// speaks, which is HTTP/1.1 when a stream is bridged onto an h1 upstream.
func StreamIdentity(method string, origin httpwire.Origin, upstream httpwire.Version) RequestIdentity {
	target := origin.WireTarget()
	return RequestIdentity{
		Method:               method,
		URL:                  origin.URL(),
		HTTPVersion:          httpwire.HTTP2.Token,
		RequestLine:          method + " " + target + " " + httpwire.HTTP2.Line,
		ForwardedRequestLine: method + " " + target + " " + upstream.Line,
	}
}

// IdentifyRequest applies an identity to the record's event document. It is
// the only place the identity fields are written; timings are not part of it
// because Commit derives them from the exchange's start. Applying an identity
// also records that the exchange has the request half of a record: Commit
// rejects an exchange that never got one.
func (e *Exchange) IdentifyRequest(identity RequestIdentity) {
	event := e.Event()
	if event == nil {
		return
	}
	e.identified = true
	event.Method = identity.Method
	event.URL = identity.URL
	event.HTTPVersion = identity.HTTPVersion
	event.RequestLine = identity.RequestLine
	event.ForwardedRequestLine = identity.ForwardedRequestLine
	event.UpstreamCertSHA256 = identity.UpstreamCertSHA256
}
