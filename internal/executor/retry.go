package executor

import "github.com/infrena/infrena/internal/retry"

// The retry loop itself lives in internal/retry, not here.
//
// It moved when `refresh` and `discovery` needed it: both make provider
// calls that a throttled account fails, both had no retry at all, and
// neither can import this package — `executor` imports `planner`, which
// imports `refresh`, so a `refresh` that imported `executor` is an import
// cycle the compiler refuses. Duplicating the loop was the alternative and
// is the worse one: two copies of a backoff schedule drift, which is the
// failure value.PlanFormatOptions' own comment records happening once
// already.
//
// RetryPolicy stays spelled that way here because it is the type of
// Options.Retry, and `retry.Policy` reads better at its definition than
// `retry.RetryPolicy` would. The alias is exact — the same type, not a
// conversion.
type RetryPolicy = retry.Policy
