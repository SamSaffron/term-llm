//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package restart

func (d *Dispatcher) Listen() func() { return d.Finish }
