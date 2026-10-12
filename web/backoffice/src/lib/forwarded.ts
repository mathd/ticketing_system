// The public scheme the gateway reports (TKT-368, extracted from login.astro).
//
// The gateway sets X-Forwarded-Proto from the real inbound request (Go's
// SetXForwarded), so behind it this says whether the browser is on https. A
// cookie marked Secure on the local plain-http stack would never be sent back,
// so a cookie is Secure exactly when this is true. Shared by the session cookie
// and the UI-language cookie, so the two cannot disagree about the scheme.
//
// The first hop only: X-Forwarded-* accumulate left to right through a proxy
// chain, and the first entry is the hop the browser addressed.
export function publicSchemeIsHttps(headers: Headers): boolean {
  return (headers.get('x-forwarded-proto') ?? '').split(',')[0]?.trim() === 'https';
}
