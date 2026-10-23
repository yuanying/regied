//go:build netns

// Package netns checks a router's behaviour from the outside, on a pseudo WAN built
// out of network namespaces.
//
// These tests do not depend on the device under test (whatever is inside the router
// netns). Every assertion is made from the client netns and the internet netns, and
// none of them look at the router's internal state. Assembling the router is confined
// to a single script, which the environment variable REGIED_NETNS_ROUTER_SETUP can
// replace (ADR 0010). That is what lets the same seven checks be applied to the
// reference implementation built from hand-written ip / nft, to an existing
// implementation, or to one of our own.
//
// The seven things checked are these.
//
//   - Outbound connectivity works over PPPoE
//   - Outbound connectivity works over DS-Lite
//   - Policy routing splits paths by source range
//   - A port forward carries traffic from outside to inside
//   - Hairpin NAT carries traffic from inside to the router's own global address
//   - The firewall drops traffic that is not allowed
//   - The NAT mapping is endpoint-independent
//
// Beyond those, the provider side delegates a prefix over DHCPv6 and moves it, and the
// checks see that what the router derives from it follows: DS-Lite, a published host
// and a subnet routed to it, and the firewall in front of them.
//
// This needs root / CAP_NET_ADMIN plus nft / pppd / pppoe-server / socat / kea-dhcp6,
// and dhcpcd for the reference router. The local development environment has none of
// them, so the usual way to run these is through a privileged container, via
// `make test-netns-docker`.
package netns
