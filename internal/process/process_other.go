//go:build !linux

package process

import "context"

func (p *Publisher) run(ctx context.Context)                { close(p.done) }
func List() ([]Record, error)                               { return nil, ErrUnsupported }
func Restart(context.Context, Record, bool) (Record, error) { return Record{}, ErrUnsupported }
