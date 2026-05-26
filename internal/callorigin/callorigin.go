// Package callorigin propagates a free-form "origin" tag through context.
// Higher-level operations tag the ctx they pass down so test doubles can
// distinguish which logical operation issued a low-level call. In production
// the tag is never read and adds no behavior.
package callorigin

import "context"

type key struct{}

// With returns a new context tagged with origin. If origin is empty the
// parent context is returned unchanged.
func With(ctx context.Context, origin string) context.Context {
	if origin == "" {
		return ctx
	}
	return context.WithValue(ctx, key{}, origin)
}

// From returns the origin tag set on ctx, or "" if none is set.
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(key{}).(string)
	return v
}
