// Command infra is the declarative infrastructure management CLI.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/infrena/infrena/internal/cli"
)

func main() {
	// SIGPIPE IS IGNORED SO THE EXIT CODE SURVIVES A READER THAT LEAVES.
	//
	// Go's runtime deliberately lets SIGPIPE kill a process that writes to a
	// broken fd 1 or 2. That is the right default for a cat-like filter, and
	// the wrong one here, for two reasons:
	//
	//  1. infrena's exit codes are a product API, documenting 0,
	//     1, 2, 3, 4 and 77, and pipelines branch on them. 141 is not one of
	//     them and tells a CI script nothing. `infrena plan dev | head -1`
	//     must still say 2 when there are changes, not 141 and not 0.
	//  2. `apply` mutates infrastructure. A signal kill walks past everything
	//     the executor does to survive interruption — it writes state under
	//     context.WithoutCancel precisely so durability outranks cancellation
	//     — and past withLockedEnvironment's release, stranding the lock for
	//     the next run. Progress output made this far more likely: an apply
	//     now writes continuously, so the pipe fills while operations are in
	//     flight rather than only at the two ends of the run.
	//
	// WHICH FAILED WRITES ARE BENIGN, AND WHICH ARE NOT:
	//
	//   - stdout (and stderr): benign. A closed pipe means the reader went
	//     away on purpose — head got its line, the pager was quit, grep -q
	//     has its answer. That is the reader's decision, not infrena's error
	//     to report, so every print site through runOutput.Out() already
	//     ignores its write error and the run carries on to its real exit
	//     code. Ignoring the signal is what turns the kill into that ignored
	//     error.
	//   - an --output FILE: NOT benign, and deliberately untouched here. A
	//     failed write there loses the machine-readable result nobody else
	//     is going to reconstruct, so report.go keeps reporting it. Note that
	//     --output also routes stdout to io.Discard, so the two can never be
	//     confused for each other.
	//
	// This is process-level disposition, so it belongs at the process entry
	// point rather than inside cli.Execute, which tests call in-process.
	signal.Ignore(syscall.SIGPIPE)

	os.Exit(cli.Execute())
}
