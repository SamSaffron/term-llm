//go:build js && wasm

package sqlitedriver

// This build has no SQLite backend. An embedding host may supply one in a
// separate integration; until then database/sql Open reports "unknown driver".
