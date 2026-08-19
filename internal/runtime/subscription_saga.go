package messageloop

import (
	"fmt"
)

// subSagaStep models one phase of a multi-step subscription mutation.
// Each step knows how to commit and how to undo itself.
// When any step fails, already-executed steps are rolled back in reverse order.
type subSagaStep struct {
	name     string
	commit   func() error
	rollback func()
}

// runSubSaga executes steps sequentially. On first commit failure it rolls
// back every previously completed step and returns a wrapped error.
func runSubSaga(steps []subSagaStep) error {
	executed := 0
	for i, step := range steps {
		if err := step.commit(); err != nil {
			for j := executed - 1; j >= 0; j-- {
				steps[j].rollback()
			}
			return fmt.Errorf("%s: %w", step.name, err)
		}
		executed = i + 1
	}
	return nil
}
