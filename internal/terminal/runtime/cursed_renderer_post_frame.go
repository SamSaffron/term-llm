package tea

import (
	"fmt"
	"io"
)

func (s *cursedRenderer) takePostFrameLocked(view *View) (string, func(error) Msg) {
	postFrame := view.PostFrame
	postFrameMsg := view.PostFrameMsg
	view.PostFrame = ""
	view.PostFrameMsg = nil
	s.view.PostFrame = ""
	s.view.PostFrameMsg = nil
	return postFrame, postFrameMsg
}

func (s *cursedRenderer) writePostFrame(postFrame string) error {
	if len(postFrame) == 0 {
		return nil
	}
	n, err := io.WriteString(s.w, postFrame)
	if err != nil {
		return fmt.Errorf("bubbletea: error writing post-frame payload: %w", err)
	}
	if n != len(postFrame) {
		return fmt.Errorf("bubbletea: error writing post-frame payload: %w", io.ErrShortWrite)
	}
	return nil
}
