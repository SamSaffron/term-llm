package chat

import "io"

func testInvocation() *chatInvocation {
	return &chatInvocation{
		opts: Options{MaxTurns: 200},
		deps: Dependencies{Stdout: io.Discard, Stderr: io.Discard},
	}
}
