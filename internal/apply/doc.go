// Package apply puts a rendered configuration on one host.
//
// The four renderers stop at text: they produce the systemd-networkd files, the nftables
// ruleset, pppd's two options files per session, and the one dnsmasq configuration, and
// none of them touches anything. This package is the other half — it reads the values
// only the running host knows, hands them to the renderers, writes what came back,
// reclaims what an earlier apply left behind, runs the reloads and restarts in the order
// ADR 0004 fixes, and puts the previous configuration back when one of them fails
// (ADR 0005).
//
// Everything it is allowed to touch outside itself is behind an interface, so the whole
// of it is exercised by unit tests that need neither root nor a network, and none of
// nft, networkctl, systemctl and pppd has to be installed to run them.
//
// A credential lives here only as long as writing the file that needs it takes, and it
// is never put into a Plan. Printing a plan therefore cannot print one (ADR 0003).
package apply
