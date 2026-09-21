package executor

import "context"

// operationContext returns the context a dispatched operation's provider
// calls run with — never Apply's own ctx.
//
// An interrupt finishes the in-flight operation rather than aborting it. A
// provider handed Apply's ctx would see it cancelled mid-call and could
// abandon a create that is physically already underway, leaving an ambiguous
// outcome. WithoutCancel keeps ctx's request-scoped values while detaching
// cancellation, so the decision to stop stays where it belongs: in whether
// Apply's loop asks the Walk for more work.
//
// It deliberately does not cover the backoff sleep between retry attempts.
// Nothing is in flight then, so cutting a backoff short and surfacing that
// attempt's error is the ordinary failure path.
func operationContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
