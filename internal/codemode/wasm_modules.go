//go:build !js

package codemode

import (
	"context"
	"fmt"
)

func (m *machine) cstring(p uint32) (string, error) {
	for n := uint32(0); n < 4096; n++ {
		b, ok := m.mod.Memory().ReadByte(p + n)
		if !ok {
			return "", fmt.Errorf("invalid module pointer")
		}
		if b == 0 {
			data, _ := m.mod.Memory().Read(p, n)
			return string(data), nil
		}
	}
	return "", fmt.Errorf("module name limit")
}
func (m *machine) moduleError(err error) uint32 {
	// The loader must report a JS exception, without retrying evaluation.
	p, e := m.alloc(err.Error())
	if e != nil {
		m.err = e
		return 0
	}
	defer m.free(p)
	v, e := m.call("qjs_new_string", p, uint64(len(err.Error())))
	if e != nil {
		m.err = e
		return 0
	}
	defer m.call("qjs_free_value", v)
	_, _ = m.call("qjs_throw", v)
	return 0
}
func (m *machine) normalize(_ context.Context, base, name uint32) uint32 {
	b, e := m.cstring(base)
	if e != nil {
		return m.moduleError(e)
	}
	s, e := m.cstring(name)
	if e != nil {
		return m.moduleError(e)
	}
	if m.modules == nil {
		return m.moduleError(fmt.Errorf("module not admitted: %s", s))
	}
	n, e := m.modules.Normalize(b, s)
	if e != nil {
		return m.moduleError(e)
	}
	p, e := m.alloc(n)
	if e != nil {
		return m.moduleError(e)
	}
	return uint32(p)
}
func (m *machine) load(_ context.Context, name, outLen uint32) uint32 {
	n, e := m.cstring(name)
	if e != nil {
		return m.moduleError(e)
	}
	if m.modules == nil {
		return m.moduleError(fmt.Errorf("module not admitted: %s", n))
	}
	s, e := m.modules.Source(n)
	if e != nil {
		return m.moduleError(e)
	}
	p, e := m.alloc(s)
	if e != nil {
		return m.moduleError(e)
	}
	if !m.mod.Memory().WriteUint32Le(outLen, uint32(len(s))) {
		m.free(p)
		return m.moduleError(fmt.Errorf("invalid module length pointer"))
	}
	return uint32(p)
}
