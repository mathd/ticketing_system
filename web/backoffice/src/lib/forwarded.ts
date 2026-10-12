// The public scheme the gateway reports (TKT-368, extracted from login.astro).
//
// What the gateway sends: its reverse proxy uses Rewrite, so Go's ReverseProxy
// removes any X-Forwarded-Proto the client sent, and SetXForwarded then sets it
// to "http" or "https" from the gateway's OWN inbound connection
// (gateway/cmd/gateway/main.go). It does not accumulate hops: the value is one
// word, and a client cannot forge it through the gateway.
//
// What this does not cover: the gateway serves plain HTTP today, so the value is
// always "http" and no cookie is Secure. If TLS is terminated by something IN
// FRONT of the gateway, the gateway still sees plain HTTP and the request reads
// as http here. Deploying behind TLS therefore needs the gateway to terminate it,
// or to trust and forward the terminator's header; this helper alone does not
// make the cookie Secure.
//
// The parsing (first comma-separated item, trimmed, exactly "https") is the
// expression login.astro already used for the session cookie, kept unchanged so
// the session cookie and the UI-language cookie cannot disagree about the
// scheme. The gateway never sends a comma-separated value; the split is
// pre-existing behaviour, not a claim about a proxy chain.
export function publicSchemeIsHttps(headers: Headers): boolean {
  return (headers.get('x-forwarded-proto') ?? '').split(',')[0]?.trim() === 'https';
}
