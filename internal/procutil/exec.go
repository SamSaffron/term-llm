package procutil

import "bytes"

type LimitedBuffer struct {
	buf   bytes.Buffer
	limit int64
	total int64
}

func NewLimitedBuffer(limit int64) *LimitedBuffer {
	if limit < 0 {
		limit = 0
	}
	return &LimitedBuffer{limit: limit}
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	b.total += int64(origLen)

	remaining := b.limit - int64(b.buf.Len())
	if remaining <= 0 {
		return origLen, nil
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return origLen, nil
}

func (b *LimitedBuffer) String() string { return b.buf.String() }

func (b *LimitedBuffer) Truncated() bool { return b.total > int64(b.buf.Len()) }
