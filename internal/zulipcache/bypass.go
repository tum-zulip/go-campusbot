package zulipcache

import (
	"context"
	"net/http"
)

type bypassKey struct{}

// WithoutCache returns a context whose requests skip the caches and always
// reach Zulip. Use it when the caller must not see a snapshot that may lag
// behind an event it is handling.
func WithoutCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, bypassKey{}, true)
}

func bypassed(req *http.Request) bool {
	bypass, _ := req.Context().Value(bypassKey{}).(bool)
	return bypass
}
