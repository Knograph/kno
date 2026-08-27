// Package value decides which Cases measure which Assets, and what that will
// cost, before any money is spent.
//
// Routing and sampling only. Nothing here invokes an agent, reads a Store, or
// writes a row — the whole package is a pure function from a run's Cases and
// Assets to a Plan, which is what makes the consent quote checkable and the
// selection auditable from a seed.
//
// The separation is not tidiness. Which Cases get measured is a SELECTION, and
// if that selection depends on a Case's recorded baseline outcome, the recorded
// outcome cannot also serve as that Case's control: reusing the draw that
// selected a Case as its control manufactures the effect being measured. See
// docs/adr/0005 for what Kno can and cannot guard, and Reserve for the
// partition that makes the control arm outcome-independent by construction.
package value
