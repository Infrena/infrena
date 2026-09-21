package executor

import "github.com/infrena/infrena/internal/retry"

// RetryPolicy is the retry behaviour Options.Retry carries.
//
// The loop itself lives in internal/retry, because refresh and discovery
// need it too and cannot import this package: executor imports planner,
// which imports refresh. The alias is exact, not a conversion.
type RetryPolicy = retry.Policy
