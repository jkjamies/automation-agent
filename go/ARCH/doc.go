// Package arch holds architecture-conformance tests for the repository.
//
// These tests parse the source tree and assert structural invariants — import
// boundaries between agents and tooling, provider-SDK isolation, and the
// presence of AGENTS.md docs — using only the standard library, so the suite
// has no external dependencies. Run with: make arch
package arch
