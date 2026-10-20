//go:build js && wasm

package sqlitedriver

// The browser demo has no SQLite engine: no driver is registered. Any Open
// attempt fails with database/sql's "unknown driver" error, which is the
// correct outcome for a page that must not touch a durable session database.
