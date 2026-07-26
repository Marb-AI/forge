// Package forge is Forge's core: the operations a front end performs, and the
// types they answer in.
//
// It is the one implementation of what Forge does. The CLI and the browser UI are
// both thin adapters over it — one parses argv and formats text, the other serves
// HTTP and JSON — and neither reimplements an operation or drives the other. That
// is why this package sits outside internal/: a front end that is not in this
// repository (a desktop or mobile shell) has to be able to import it, and a core
// under internal/ can never be imported from outside.
//
// The direction of the dependency is the whole point. Until now the operations
// lived in the cli package and the UI borrowed them, which made the CLI the
// definition of Forge and the UI a guest — with the standing temptation of having
// the UI shell out to `forge`, impossible on a phone and untestable everywhere.
// Now both depend on this package and this package depends on neither.
//
// The types here carry JSON tags because this core is what serves the browser: the
// wire format the UI consumes is defined once, here, rather than translated on the
// way out.
package forge
