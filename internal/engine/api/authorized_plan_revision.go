package api

import "context"

type authorizedPlanRevisionContextKey struct{}

// ContextWithAuthorizedPlanRevision binds the plan snapshot inspected by the
// authorization middleware to the downstream apply handler. Only the numeric
// revision is carried; actions and permissions remain repository-owned.
func ContextWithAuthorizedPlanRevision(ctx context.Context, revision int) context.Context {
	return context.WithValue(ctx, authorizedPlanRevisionContextKey{}, revision)
}

func AuthorizedPlanRevisionFromContext(ctx context.Context) (int, bool) {
	revision, ok := ctx.Value(authorizedPlanRevisionContextKey{}).(int)
	return revision, ok && revision > 0
}
