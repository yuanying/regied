// Package nftables builds the nftables ruleset regied owns out of a validated
// configuration.
//
// It renders and nothing else. No kernel is read, no nft command is run, and nothing is
// taken from the running host. The result is a function of the configuration alone, so
// the tests need neither privileges nor an uplink, and the same configuration always
// produces the same text. The one value a configuration cannot hold — the address an
// uplink is holding, which the hairpin half of a port forward matches on — is not in the
// text at all: the ruleset declares a set per uplink and family and matches on that, and
// whoever learns an address puts it in (ADR 0015). Applying it — replacing the table,
// seeding those sets, rolling back, showing a difference — belongs to the apply engine.
//
// Everything here lives in one table, and regied touches nothing outside it. The ruleset
// is never flushed (ADR 0009).
package nftables
