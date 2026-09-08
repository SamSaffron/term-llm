//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package restart

func (d *Coordinator) Listen() func() { return func() {} }
