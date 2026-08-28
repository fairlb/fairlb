package video

import "github.com/fairlb/fairlb/internal/gateway/catalog"

// BillingUnits turns an admitted request into the quantity vector it will be
// charged on.
//
// This is the function ADR-0220 rests on: every input is a parameter the caller
// wrote and this gateway already validated against the envelope, so the amount
// derived from it before any upstream is called is the amount that will be
// settled. Nothing here reads an upstream response, and nothing estimates.
//
// The resolution and audio axes are carried into the key rather than collapsed,
// because a rate card that prices 1080p-with-sound differently from 720p-silent
// has to be able to say so. A model with one flat per-second price simply has a
// single row with those axes empty, and UnitPriceTable.Lookup widens to it.
func BillingUnits(r Request, audioOn bool, unit catalog.Unit) catalog.Units {
	key := catalog.UnitKey{
		Unit:       unit,
		Resolution: r.Resolution,
		Audio:      audioFlag(audioOn),
	}
	n := int64(r.N)
	if n < 1 {
		n = 1
	}
	quantity := n
	if unit == catalog.UnitSecond {
		quantity = int64(r.DurationSeconds) * n
	}
	return catalog.Units{Quantities: map[catalog.UnitKey]int64{key: quantity}}
}

// audioFlag spells the axis the way the rate rows do. The empty string is
// reserved for a rate that does not vary on audio and is never produced here:
// a request has always been resolved to a definite answer by this point.
func audioFlag(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
