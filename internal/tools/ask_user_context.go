package tools

import "context"

type askUserUIContextKey struct{}

// AskUserContextUIFunc renders ask_user questions using a request-scoped UI
// implementation, such as the web serve roundtrip.
type AskUserContextUIFunc func(context.Context, []AskUserQuestion) ([]AskUserAnswer, error)

// ContextWithAskUserUIFunc stores a request-scoped ask_user handler in ctx.
func ContextWithAskUserUIFunc(ctx context.Context, fn AskUserContextUIFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, askUserUIContextKey{}, fn)
}

func askUserUIFuncFromContext(ctx context.Context) AskUserContextUIFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(askUserUIContextKey{}).(AskUserContextUIFunc)
	return fn
}
