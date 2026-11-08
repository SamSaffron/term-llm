// Package procutil owns child-process lifetime, bounded output capture, shell
// environment rendering, and Unix tagged-descendant cleanup. Callers prepare a
// Command, then Start/Run/Wait it and Close it to release the owned tree.
package procutil
