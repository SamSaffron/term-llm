package tea

import "time"

// wakeRenderer coalesces work notifications without blocking the caller. The
// channel lives across renderer restarts; each joined loop owns its own ticker.
func (p *Program) wakeRenderer() {
	select {
	case p.rendererWake <- struct{}{}:
	default:
	}
}

// startRenderer starts one renderer loop. Repeated starts while the loop is
// live are no-ops; an exited loop is joined and closed before replacement.
func (p *Program) startRenderer() {
	p.rendererLifecycleMu.Lock()
	defer p.rendererLifecycleMu.Unlock()

	if p.renderer == nil {
		return
	}
	if p.rendererLoopDone != nil {
		select {
		case <-p.rendererLoopDone:
			p.rendererStop = nil
			p.rendererLoopDone = nil
			if !p.rendererClosed {
				p.renderer.beginShutdown()
				_, _ = p.renderer.flush(true)
				_ = p.renderer.close()
				p.rendererClosed = true
			}
		default:
			return
		}
	}

	p.renderer.start()
	p.rendererClosed = false
	stop := make(chan struct{})
	done := make(chan struct{})
	p.rendererStop = stop
	p.rendererLoopDone = done
	framerate := time.Second / time.Duration(p.fps)
	idleFrames := max(p.fps/2, 1)
	ticker := time.NewTicker(framerate)
	renderer := p.renderer
	go func() {
		defer close(done)
		defer ticker.Stop()
		defer func() {
			if r := recover(); r != nil {
				p.recoverFromGoPanic(r)
			}
		}()
		var (
			idle            int
			parked, realign bool
			last            time.Time
		)
		for {
			select {
			case <-stop:
				return
			case <-p.rendererWake:
				if parked {
					// Keep the same frame boundaries as the continuously running ticker.
					parked, realign = false, true
					ticker.Reset(framerate - time.Since(last)%framerate)
				}
			case last = <-ticker.C:
				if realign {
					realign = false
					ticker.Reset(framerate)
				}
				_ = p.flush()
				if active, _ := renderer.flush(false); active {
					idle = 0
				} else if idle++; idle >= idleFrames {
					ticker.Stop()
					parked = true
				}
			}
		}
	}()
}

// stopRenderer stops and joins the renderer loop before flushing or closing
// renderer state. If kill is true, the final frame is discarded.
func (p *Program) stopRenderer(kill bool) {
	p.rendererLifecycleMu.Lock()
	defer p.rendererLifecycleMu.Unlock()

	if p.renderer == nil || p.rendererClosed {
		return
	}
	p.renderer.beginShutdown()
	if p.rendererLoopDone != nil {
		close(p.rendererStop)
		<-p.rendererLoopDone
		p.rendererStop = nil
		p.rendererLoopDone = nil
	}
	if !kill {
		_, _ = p.renderer.flush(true)
	}
	_ = p.renderer.close()
	p.rendererClosed = true
}
