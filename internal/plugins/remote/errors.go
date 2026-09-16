package remote

import (
	"fmt"
	"time"
)

// TokenVar is the variable infrena reads first for a forge token, and
// GitHubTokenVar the one it falls back to. They are named in error messages
// because setting one is the action that fixes a rate limit today rather than
// in an hour.
const (
	TokenVar       = "INFRENA_GITHUB_TOKEN"
	GitHubTokenVar = "GITHUB_TOKEN"
)

// RateLimitError says the forge refused because this caller has asked too
// often, NOT because the thing asked for is missing.
//
// THE DISTINCTION IS THE POINT. GitHub answers an exhausted limit with 403 and
// a missing-or-private repository with 404, so both arrive from the same call,
// and an unauthenticated caller gets sixty requests an hour, which one owner
// search with a handful of candidates can spend in a single invocation.
// Reporting that as "not found" tells a user their plugin does not exist, sends
// them to check a spelling that was right, and hides a condition that clears
// itself in under an hour.
type RateLimitError struct {
	// Resets is when the limit refills, from X-RateLimit-Reset. Zero when the
	// forge did not say.
	Resets time.Time
	// Authenticated records whether a token was sent. An unauthenticated caller
	// has a much smaller allowance, so the suggested action differs: set a
	// token, rather than wait.
	Authenticated bool
}

// Error reports the limit, when it lifts, and the action the reader can take.
func (e *RateLimitError) Error() string {
	msg := "github rate limit exceeded"
	if !e.Resets.IsZero() {
		msg += fmt.Sprintf(", resets at %s (in %s)",
			e.Resets.Format(time.RFC3339), time.Until(e.Resets).Round(time.Second))
	}
	if e.Authenticated {
		return msg + ": wait until it resets, or search fewer sources at once. " +
			"The token in " + TokenVar + " or " + GitHubTokenVar + " is already being used"
	}
	return msg + ": unauthenticated searches get sixty requests an hour. " +
		"Set " + TokenVar + " (or " + GitHubTokenVar + ") to a personal access token to raise it, or wait"
}

// NotFoundError says the forge has no such thing, which on GitHub also covers
// a private repository an unauthenticated caller cannot see.
type NotFoundError struct {
	// What names the thing that is missing, so the message says where to look.
	What string
}

// Error reports what is missing and what a reader can do about it.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s not found: check the spelling, or set %s if it is private",
		e.What, TokenVar)
}

// ForbiddenError is a 403 that is NOT a rate limit: a private repository, a
// token without the scope, or a revoked one. It is its own type because
// collapsing it into either of the two above sends the reader somewhere
// useless.
type ForbiddenError struct {
	// What names the thing that was refused.
	What string
	// Message is whatever the forge said, when it said anything.
	Message string
}

// Error reports the refusal, and that a token is the usual remedy.
func (e *ForbiddenError) Error() string {
	msg := fmt.Sprintf("%s refused by github", e.What)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg + ": this is not a rate limit. The repository may be private, or the token in " +
		TokenVar + " may lack access"
}

// APIError is any other unexpected status. It carries the code so a reader can
// tell a forge outage from a bug here.
type APIError struct {
	// What names the thing that was asked for.
	What string
	// Status is the HTTP status code the forge returned.
	Status int
	// Message is whatever the forge said, when it said anything.
	Message string
}

// Error reports the status and what was being read at the time.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("github returned %d for %s", e.Status, e.What)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg + ": expected 200. Try again, and check https://www.githubstatus.com if it persists"
}
