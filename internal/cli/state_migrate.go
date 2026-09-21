package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/infrena/infrena/internal/config"
	"github.com/infrena/infrena/internal/state"
	"github.com/infrena/infrena/pkg/report"
)

// migrationStatus is what the comparison of two backends came to, in words.
// The exit code a pipeline reads is derived from this and never the other way
// round, so nothing here has to map a number back to a meaning.
type migrationStatus string

const (
	// migrationNotConfigured: no `migrate_from:` block, so there is no
	// migration to have an opinion about. Every project says this.
	migrationNotConfigured migrationStatus = "none"
	// migrationPending: the source holds state the destination does not.
	migrationPending migrationStatus = "pending"
	// migrationComplete: both ends already agree, so the block is stale.
	migrationComplete migrationStatus = "complete"
	// migrationConflict: both ends hold state for the same environment and it
	// is not the same state. Somebody has been applying to one of them.
	migrationConflict migrationStatus = "conflict"
	// migrationFailed: the question could not be answered at all — a backend
	// that would not open, configuration that would not read. Distinct from
	// every answer above, because a consumer that read "nothing to do" from a
	// question nobody answered would carry on past a pending migration.
	migrationFailed migrationStatus = "error"
)

// migrationComparison is the three-case decision, made once by compareEnds and
// consumed by both the migration and `--check`. A second implementation of "are
// these two ends the same" is how a check comes to report one thing and the
// migration then do another.
type migrationComparison struct {
	Status migrationStatus
	// Pending environments the source holds and the destination does not.
	Pending []string
	// Complete environments already identical at both ends.
	Complete []string
	// Conflict environments both ends hold, differently.
	Conflict []string
	// source is each environment's state as the source holds it, read during
	// the comparison so the copy does not have to read it a second time and
	// possibly get something else.
	source map[string]*state.State
}

// Environments names the environments this status is about, which is not every
// environment involved: naming the ones that agree alongside a conflict buries
// the ones a person has to look at.
func (c migrationComparison) Environments() []string {
	switch c.Status {
	case migrationPending:
		return c.Pending
	case migrationConflict:
		return c.Conflict
	default:
		return c.Complete
	}
}

// compareEnds decides which of the three situations a migration is in.
//
// It writes nothing and takes no lock, which is what lets `--check` call it
// unchanged. A caller that is about to write takes the locks first and then
// calls this, so the comparison it acts on is made under them.
//
// Comparison is over encoded bytes rather than field by field, because a
// backend stores bytes and this command must not start interpreting them. The
// exception is Serial and UpdatedAt, which a write stamps and a copy therefore
// advances; comparing them raw would make every successful migration look like
// a conflict on the next run.
func compareEnds(ctx context.Context, source, destination state.Backend) (migrationComparison, error) {
	environments, err := source.List(ctx)
	if err != nil {
		return migrationComparison{}, fmt.Errorf("listing the environments the source backend holds: %w", err)
	}
	sort.Strings(environments)

	c := migrationComparison{source: map[string]*state.State{}}
	for _, environment := range environments {
		src, err := source.Get(ctx, environment)
		if err != nil {
			return migrationComparison{}, fmt.Errorf("reading %q from the source backend: %w", environment, err)
		}
		dst, err := destination.Get(ctx, environment)
		if err != nil {
			return migrationComparison{}, fmt.Errorf("reading %q from the destination backend: %w", environment, err)
		}

		same, err := sameState(src, dst)
		if err != nil {
			return migrationComparison{}, err
		}
		c.source[environment] = src
		switch {
		case same:
			c.Complete = append(c.Complete, environment)
		case len(dst.Resources) == 0:
			c.Pending = append(c.Pending, environment)
		default:
			c.Conflict = append(c.Conflict, environment)
		}
	}

	// A conflict anywhere makes the whole migration a conflict: the decision
	// is about the migration, not about one environment, and copying the
	// agreeable half while refusing the rest leaves a person reasoning about
	// two backends that are each half right.
	switch {
	case len(c.Conflict) > 0:
		c.Status = migrationConflict
	case len(c.Pending) > 0:
		c.Status = migrationPending
	default:
		c.Status = migrationComplete
	}
	return c, nil
}

// sameState reports whether two ends hold the same state, comparing the bytes.
func sameState(a, b *state.State) (bool, error) {
	encodedA, err := comparableBytes(a)
	if err != nil {
		return false, err
	}
	encodedB, err := comparableBytes(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(encodedA, encodedB), nil
}

// comparableBytes encodes state with the write stamps cleared. Serial and
// UpdatedAt are set by whichever backend performed the write, so a faithful
// copy necessarily carries different values for both; nothing else is touched.
//
// The copy is shallow, which is safe because only those two scalars change:
// Resources is shared with the caller's state and never written through.
func comparableBytes(s *state.State) ([]byte, error) {
	normalised := *s
	normalised.Serial = 0
	normalised.UpdatedAt = time.Time{}
	return normalised.Encode()
}

// newStateMigrateCommand builds `infrena state migrate`, which copies state
// from the backend `migrate_from:` names to the one `backend:` names.
//
// Both ends are locked for the whole operation, and both are compared before
// anything is written, because discovering which of three situations this is
// halfway through a copy is too late:
//
//	source has state, destination empty      -> migrate
//	source has state, destination identical  -> no-op, succeed
//	source has state, destination differs    -> refuse
//
// The middle case is what makes a re-run safe. A migration run through CI gets
// re-run, and a failure there invites somebody to add --force to the workflow,
// where it stays forever — which is also why the refusal below does not lead
// with --force.
//
// Every environment is verified by reading it back from the destination: a
// backend that accepted a write and cannot return it has lost state.
//
// Any failure leaves the source authoritative, because nothing is ever removed
// from it. Emptying the source is the user's decision, and until they make it
// the old state lingers with every secret recorded in it.
func newStateMigrateCommand(opts *GlobalOptions) *cobra.Command {
	var force, check bool
	cmd := &cobra.Command{
		Use:           "migrate",
		SilenceUsage:  true,
		SilenceErrors: true,
		Short:         "Copy state from the backend `migrate_from:` names into the one `backend:` names",
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check {
				return runStateMigrateCheck(cmd, opts)
			}
			return runStateMigrate(cmd, opts, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"overwrite state the destination already holds, when the two ends differ")
	cmd.Flags().BoolVar(&check, "check", false,
		"report whether a migration is needed and change nothing; the exit code is the answer")
	return cmd
}

// The outcomes `--check` reports, carried as sentinel errors the way this
// package already carries errChanges and errNoApproval: RunE returns an error,
// and root.go's Execute is the one place that turns an outcome into a number.
var (
	// errMigrationPending: the source holds state the destination does not.
	// Exit 2, the same code `plan` uses, because it means the same thing —
	// something is pending, run the corresponding command.
	errMigrationPending = errors.New("a migration is pending")
	// errMigrationComplete: both ends agree and the block is stale. Exit 3.
	errMigrationComplete = errors.New("the migration is already complete")
	// errMigrationConflict: both ends hold different state. Exit 4, and unlike
	// the others this one is printed, because it sends for a person who needs
	// to know what for.
	errMigrationConflict = errors.New("the two backends hold different state")
)

// runStateMigrateCheck answers the question the migration acts on, without
// acting on it: it writes nothing and takes no lock, so a lock it left behind
// cannot block the migration it just recommended.
//
// It calls compareEnds, the same function the migration decides on, rather than
// a read-only re-implementation.
func runStateMigrateCheck(cmd *cobra.Command, opts *GlobalOptions) error {
	ro, closeRun, err := openRun(cmd, opts, "state migrate", "")
	if err != nil {
		return err
	}
	defer closeRun()

	destinationDecl, sourceDecl := backendDecls(opts.Dir)
	comparison, err := checkMigration(cmd.Context(), opts, destinationDecl, sourceDecl)
	if err != nil {
		writeMigrateResult(ro, migrationFailed, nil, err)
		return err
	}
	writeMigrateResult(ro, comparison.Status, comparison.Environments(), nil)

	out := ro.Out()
	switch comparison.Status {
	case migrationNotConfigured:
		fmt.Fprint(out, "This project has no `migrate_from:` block, so no migration is pending.\n")
		return nil
	case migrationPending:
		fmt.Fprintf(out, "A migration is pending: state for %s is at %s and not at %s.\n"+
			"Run `infrena state migrate` to copy it.\n",
			joinEnvironments(comparison.Pending),
			backendName(sourceDecl), backendName(destinationDecl))
		return errMigrationPending
	case migrationComplete:
		reportAlreadyMigrated(out, comparison)
		return errMigrationComplete
	default:
		return conflictError(comparison, backendName(sourceDecl), backendName(destinationDecl))
	}
}

// checkMigration opens both ends read-only and compares them.
//
// An absent `migrate_from:` is not an error here, the way it is for the
// migration itself: "is a migration needed" has an answer for every project,
// and for almost every project the answer is no.
func checkMigration(
	ctx context.Context, opts *GlobalOptions, destinationDecl, sourceDecl config.BackendDecl,
) (migrationComparison, error) {
	if sourceDecl.Plugin == "" {
		if sourceDecl.Origin.File != "" {
			// A block that was written and could not be read is not an
			// absent block, and answering "nothing is pending" for one would
			// send a pipeline past a migration nobody can see.
			return migrationComparison{}, checkMigrateFrom(sourceDecl)
		}
		return migrationComparison{Status: migrationNotConfigured}, nil
	}

	source, closeSource, err := openBackend(ctx, opts, sourceDecl)
	if err != nil {
		return migrationComparison{}, fmt.Errorf("opening the backend `migrate_from:` names: %w", err)
	}
	defer closeSource()
	destination, closeDestination, err := openBackend(ctx, opts, destinationDecl)
	if err != nil {
		return migrationComparison{}, fmt.Errorf("opening the backend `backend:` names: %w", err)
	}
	defer closeDestination()

	return compareEnds(ctx, source, destination)
}

// writeMigrateResult writes the check's one machine-readable line, carrying the
// status as a word so a consumer never maps a number back to a meaning. Like
// every other result line here it is best effort: with no --output there is no
// report at all.
func writeMigrateResult(ro *runOutput, status migrationStatus, environments []string, err error) {
	rw := ro.Report()
	if rw == nil {
		return
	}
	result := report.MigrateResult{Status: string(status), Environments: environments}
	if err != nil {
		result.Error = err.Error()
	}
	_ = rw.WriteMigrateResult(result)
}

func runStateMigrate(cmd *cobra.Command, opts *GlobalOptions, force bool) error {
	destinationDecl, sourceDecl := backendDecls(opts.Dir)
	if err := checkMigrateFrom(sourceDecl); err != nil {
		return err
	}

	ctx := state.WithOperation(cmd.Context(), "state migrate")
	source, closeSource, err := openBackend(ctx, opts, sourceDecl)
	if err != nil {
		return fmt.Errorf("opening the backend `migrate_from:` names: %w", err)
	}
	defer closeSource()
	destination, closeDestination, err := openBackend(ctx, opts, destinationDecl)
	if err != nil {
		return fmt.Errorf("opening the backend `backend:` names: %w", err)
	}
	defer closeDestination()

	// Both ends, for the whole operation: a comparison made without the locks
	// is one something else can invalidate between the decision and the copy.
	environments, err := source.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the environments the source backend holds: %w", err)
	}
	sort.Strings(environments)
	release, err := lockBothEnds(ctx, source, destination, environments)
	if err != nil {
		return err
	}
	defer release()

	comparison, err := compareEnds(ctx, source, destination)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	switch comparison.Status {
	case migrationConflict:
		if !force {
			return conflictError(comparison, backendName(sourceDecl), backendName(destinationDecl))
		}
	case migrationComplete:
		reportAlreadyMigrated(out, comparison)
		return nil
	}

	copying := append(append([]string{}, comparison.Pending...), comparison.Conflict...)
	sort.Strings(copying)
	fmt.Fprintf(out, "Copying state from %s to %s.\n",
		backendName(sourceDecl), backendName(destinationDecl))
	for _, environment := range copying {
		if err := copyEnvironment(ctx, destination, environment, comparison.source[environment]); err != nil {
			// The source is untouched, so the old backend is still the
			// record, and saying so is what tells a reader they have lost
			// nothing.
			return fmt.Errorf("%w\nNothing was removed from %s, so it still holds this project's state.",
				err, backendName(sourceDecl))
		}
		fmt.Fprintf(out, "  %s: copied and verified\n", environment)
	}

	fmt.Fprintf(out, "\nMigrated %s to %s.\n",
		countedEnvironments(len(copying)), backendName(destinationDecl))
	fmt.Fprintf(out, "%s still holds its copy: migration copies rather than moves, so you can point\n"+
		"`backend:` back if anything is wrong. Emptying it is yours to do, and until you do\n"+
		"it holds this project's state, secrets included.\n", backendName(sourceDecl))
	reportRemovalIsOptional(out)
	return nil
}

// checkMigrateFrom refuses a migration nobody configured, saying what to add.
//
// An empty plugin with an origin is not an absent block: `migrate_from:` was
// written and could not be read, and reporting that as "there is no such block"
// would send a user to add one they can already see.
func checkMigrateFrom(decl config.BackendDecl) error {
	if decl.Plugin != "" {
		return nil
	}
	if decl.Origin.File != "" {
		return fmt.Errorf(
			"this project's `migrate_from:` block could not be read, so infrena cannot tell where to migrate from\n"+
				"  declared in: %s\n"+
				"Run `infrena validate` to see what is wrong with it.", decl.Origin.File)
	}
	return fmt.Errorf(
		"this project has no `migrate_from:` block, so there is nothing to migrate from\n" +
			"A migration needs both ends configured. Add a `migrate_from:` block beside\n" +
			"`backend:`, taking the same shape, and naming the backend the state is in today:\n" +
			"\n" +
			"  migrate_from:\n" +
			"    plugin: local\n" +
			"\n" +
			"`plugin: local` names the built-in backend; any installed backend can be named\n" +
			"instead, with its own settings.")
}

// lockBothEnds takes every environment's lock at both ends and returns the
// release.
//
// Locks are taken in a stable order and released in reverse, and a failure
// releases everything already taken: a migration that gave up holding half the
// locks it took would leave environments nobody can use.
func lockBothEnds(ctx context.Context, source, destination state.Backend, environments []string) (func(), error) {
	type held struct {
		backend     state.Backend
		environment string
	}
	var taken []held
	release := func() {
		for i := len(taken) - 1; i >= 0; i-- {
			_ = taken[i].backend.Unlock(ctx, taken[i].environment)
		}
	}

	for _, environment := range environments {
		for _, end := range []struct {
			backend state.Backend
			which   string
		}{{source, "source"}, {destination, "destination"}} {
			if _, err := end.backend.Lock(ctx, environment); err != nil {
				release()
				return nil, fmt.Errorf("locking %q at the %s backend: %w\n"+
					"A migration holds both ends for the whole copy, so it cannot start while\n"+
					"another run holds either one. Wait for that run, or release the lock with\n"+
					"`infrena state unlock %s` once you know it has finished.",
					environment, end.which, err, environment)
			}
			taken = append(taken, held{end.backend, environment})
		}
	}
	return release, nil
}

// copyEnvironment writes one environment to the destination and reads it back.
// A backend that accepts a write and cannot return it has lost state, and a
// migration that trusted the write would report success over the loss.
func copyEnvironment(ctx context.Context, destination state.Backend, environment string, src *state.State) error {
	want, err := comparableBytes(src)
	if err != nil {
		return err
	}
	if err := destination.Put(ctx, environment, src); err != nil {
		return fmt.Errorf("writing %q to the destination backend: %w", environment, err)
	}
	stored, err := destination.Get(ctx, environment)
	if err != nil {
		return fmt.Errorf("reading %q back from the destination backend: %w", environment, err)
	}
	got, err := comparableBytes(stored)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf(
			"the destination backend accepted %q and returned something else when asked for it again\n"+
				"A backend that cannot return what it stored has lost state, so the migration stopped here.",
			environment)
	}
	return nil
}

// conflictError is the refusal when both ends hold different state.
//
// --force is mentioned last, after the reader has been told how to find out
// which end is right, because the first remedy offered is the one that ends up
// in a workflow file, where it sits on every future run and silently overwrites
// the next real conflict.
func conflictError(c migrationComparison, source, destination string) error {
	return fmt.Errorf(
		"%w: %s and %s both hold state for %s\n"+
			"  source:      %s\n"+
			"  destination: %s\n"+
			"Somebody has applied to one of these since the migration was configured, so one\n"+
			"of the two is the real record of what exists and infrena cannot tell which.\n"+
			"Look at both with `infrena state list <environment>` before deciding: run it\n"+
			"once with the current configuration and once with `backend:` set to %s.\n"+
			"When you know the destination's copy is the one to discard, re-run this command\n"+
			"with --force.",
		errMigrationConflict, source, destination, joinEnvironments(c.Conflict),
		source, destination, source)
}

// reportAlreadyMigrated is the no-op success. A success rather than a failure,
// because CI re-runs happen and a failure here is what puts --force in a
// workflow file forever.
func reportAlreadyMigrated(out io.Writer, c migrationComparison) {
	if len(c.Complete) == 0 {
		fmt.Fprint(out, "The source backend holds no state, so there is nothing to migrate.\n")
	} else {
		fmt.Fprintf(out, "Both ends already hold the same state for %s: already migrated, nothing to do.\n",
			joinEnvironments(c.Complete))
	}
	reportRemovalIsOptional(out)
}

// reportRemovalIsOptional closes every outcome with the tidy-up and with the
// fact that not doing it costs nothing. In CI this lands in a log nobody reads,
// where "remove the block" on its own would look like an outstanding failure to
// whoever eventually finds it.
func reportRemovalIsOptional(out io.Writer) {
	fmt.Fprint(out, "You can now remove the `migrate_from:` block. Leaving it is harmless:\n"+
		"no command but `state migrate` acts on it.\n")
}

// backendName is what to call a backend in a message a person reads.
func backendName(decl config.BackendDecl) string {
	if decl.Plugin == "" {
		return localBackendName
	}
	return decl.Plugin
}

func joinEnvironments(environments []string) string {
	switch len(environments) {
	case 0:
		return "no environments"
	case 1:
		return environments[0]
	}
	return fmt.Sprintf("%s and %s",
		joinList(environments[:len(environments)-1]), environments[len(environments)-1])
}

func joinList(environments []string) string {
	out := ""
	for i, e := range environments {
		if i > 0 {
			out += ", "
		}
		out += e
	}
	return out
}

func countedEnvironments(n int) string {
	if n == 1 {
		return "1 environment"
	}
	return fmt.Sprintf("%d environments", n)
}
