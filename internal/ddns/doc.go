// Package ddns writes DNS records at a provider so that they hold an address this host's
// uplink holds (ADR 0019).
//
// It has two halves and they are in two files. The Writer decides and remembers: which
// record to write, whether to create it or update it, and what it last put there. The
// Cloudflare client is the one thing that speaks the provider's API.
//
// The provider is behind an interface, and that interface is a test seam rather than the
// beginning of a second provider. ADR 0019 decided there is one provider and no provider
// abstraction; what this buys is that every decision above can be exercised by a unit
// test that opens no socket.
//
// Nothing here knows about the configuration document. The apply engine builds a Record
// out of a declaration and what the kernel says, and hands it over.
package ddns
