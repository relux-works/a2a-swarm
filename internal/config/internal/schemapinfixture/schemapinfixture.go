// Package schemapinfixture declares types that equal declarations in package
// config in name and structure and differ only in a package path: of a named
// type's identity, of an unexported struct field, or of an unexported
// interface method. Only the config schema-pin tests import it.
package schemapinfixture

// Pos has the name and structure of a config-local `type Pos int`.
type Pos int

// Hidden is struct{ a int } whose unexported field a belongs to this package.
type Hidden = struct{ a int }

// Sealed is interface{ m() } whose unexported method m belongs to this package.
type Sealed = interface{ m() }
