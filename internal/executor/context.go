package executor

import "context"

// operationContext returns the context a single dispatched operation's
// provider calls must run with — never Apply's own ctx directly.
//
// Spec §15: a SIGINT "finishes the in-flight operation" rather than
// aborting it. If dispatch handed a provider call Apply's own ctx, a
// well-behaved provider — including the fake provider, whose delay honors
// ctx exactly as a real SDK client would — could see that ctx canceled
// mid-call and abort a Create that is physically already underway. That is
// the same ambiguous-outcome risk RetryPolicy's "create is never retried on
// an ambiguous failure" rule (task 4) already exists to keep out of this
// system, arriving through cancellation instead of a retry.
//
// context.WithoutCancel keeps any request-scoped values ctx carries while
// detaching cancellation, so a dispatched operation's own provider call
// cannot observe the coordinator's decision to stop. That decision stays
// visible only where it belongs: at the top of Apply's loop, in whether it
// calls Walk.Ready() again — never inside a call already handed to a
// provider.
//
// This does NOT apply to RetryPolicy.Sleep (task 4): nothing is in flight
// during a backoff sleep between attempts (the previous provider call
// already returned), so Sleep should keep receiving ctx directly — a
// SIGINT cutting a backoff short and surfacing that attempt's classified
// error is exactly the ordinary failure path task 9 already handles.
//
// internal/executor has a second, unrelated call to context.WithoutCancel:
// record's call to Backend.Put (apply.go), which detaches the durability
// write of final state from the same cancellation, so the write that
// preserves what already happened cannot itself be cut short by the signal
// that triggered it. The two are kept as separate calls rather than one
// shared helper on purpose — they protect different things (an in-flight
// provider call here vs. a durability write there) for different reasons,
// and a single helper whose doc has to justify both would be worse than
// two with one reason each. See apply.go's record for its side of this
// cross-reference. If a third call site ever appears, that is the point to
// extract a shared helper — not before.
func operationContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
