package core

// ServiceRate is a service's admin-owned pricing.
//
// Both fields are one decision and are kept together deliberately. `Raw` is not
// a display preference sitting beside a price — it CHANGES the price: a raw job
// costs a flat credit, where a units job costs CreditsPerUnit per output unit.
// Reading the number without the flag renders a figure that is wrong for every
// raw service, and no type would have stopped that if they lived apart.
//
// ⚠ Neither half is ever derived from a worker. ADR-0001 split label validity
// (derived from live workers, harmless if wrong) from pricing (admin-owned),
// because a worker runs on a host the operator does not control and must not be
// able to set what customers are charged. ADR-0006 keeps the mode on the
// pricing side of that split for the same reason.
type ServiceRate struct {
	// CreditsPerUnit is what one output unit costs. A label with no row costs 1:
	// 0 would make an unconfigured service silently free.
	CreditsPerUnit int
	// Raw means the service's output is opaque bytes rather than a list of
	// units, and the job bills a flat 1 credit.
	Raw bool
}

// MaxCreditsPerUnit bounds a service's rate (F8, REPORT 2026-11-10).
//
// The charge is len(units) x rate in int arithmetic; a rate near 1<<62 wraps
// it negative, and delivery books a negative charge as a CREDIT. A million
// credits per unit is far beyond any real price and leaves the product
// representable for any output a worker can produce.
const MaxCreditsPerUnit = 1_000_000
