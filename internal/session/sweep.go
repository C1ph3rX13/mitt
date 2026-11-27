package session

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/C1ph3rX13/mitt/internal/atomicfile"
)

// SweepStaleTemps removes crash-left temporary files from every data directory
// this store writes into and reports the outcome to stderr. Record writers call
// it once at startup, before their first Begin, so residue from a crashed run
// cannot accumulate. A failure is a warning, never a startup failure: hygiene
// must not block capture.
func (s *Store) SweepStaleTemps(stderr io.Writer) {
	report, _ := s.sweepStaleTemps(atomicfile.StaleTempMaxAge)
	for _, failure := range report.failures {
		warnSweep(stderr, "sweeping stale temporary files in %s: %v", failure.dir, failure.err)
	}
	for _, dir := range report.dirs {
		if count := report.removedByDir[dir]; count > 0 {
			warnSweep(stderr, "removed %d stale temporary file(s) from %s", count, dir)
		}
	}
}

// tempSweepReport is what one pass over a store's directories changed.
type tempSweepReport struct {
	// dirs lists the directories swept, in sweep order.
	dirs []string
	// removedByDir counts removed temporaries per directory.
	removedByDir map[string]int
	// inUse lists the temporaries kept because a live writer holds them.
	inUse []string
	// failures lists the directories a sweep could not finish.
	failures []tempSweepFailure
}

// empty reports whether the sweep changed or found nothing.
func (r tempSweepReport) empty() bool {
	return len(r.removedByDir) == 0 && len(r.inUse) == 0
}

type tempSweepFailure struct {
	dir string
	err error
}

// err joins every sweep failure, so a caller that wants to treat residue as an
// error (tests do) can; the command paths report each failure as a warning.
func (r tempSweepReport) err() error {
	errs := make([]error, 0, len(r.failures))
	for _, failure := range r.failures {
		errs = append(errs, fmt.Errorf("%s: %w", failure.dir, failure.err))
	}
	return errors.Join(errs...)
}

// sweepStaleTemps runs the persistence protocol's sweep over the store root,
// the bodies directory, and the CA directory. The root holds proxy.json
// temporaries, bodies holds staged entity bodies, and ca holds CA key and
// certificate temporaries; all of them share the protocol's naming, lock, and
// age rule, so one sweeper covers every writer instead of one per file kind.
// Live writers are protected by their per-file lock wherever the file lives.
func (s *Store) sweepStaleTemps(maxAge time.Duration) (tempSweepReport, error) {
	report := tempSweepReport{removedByDir: map[string]int{}}
	for _, dir := range []string{s.dir, s.BodiesDir(), s.CADir()} {
		swept, err := atomicfile.Sweep(dir, maxAge)
		report.dirs = append(report.dirs, dir)
		if len(swept.Removed) > 0 {
			report.removedByDir[dir] = len(swept.Removed)
		}
		report.inUse = append(report.inUse, swept.InUse...)
		if err != nil {
			report.failures = append(report.failures, tempSweepFailure{dir: dir, err: err})
		}
	}
	return report, report.err()
}

func warnSweep(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "mitt: warning: "+format+"\n", args...)
}
