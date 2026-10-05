package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Solver runs the airtime solver as a subprocess.
//
// A subprocess rather than a linked library so that a crash or a runaway solve
// cannot take the AAA server down with it, and so an operator can replay a
// captured problem by hand when a subscriber complains. The cost is one fork and
// a JSON round trip per tick, which at a 15-second tick is irrelevant.
type Solver struct {
	path    string
	timeout time.Duration
}

// NewSolver returns a solver that runs the binary at path.
func NewSolver(path string, timeout time.Duration) *Solver {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Solver{path: path, timeout: timeout}
}

// ErrStalePlan means the solver answered a different problem than the one sent,
// which should be impossible and must never be applied.
var ErrStalePlan = errors.New("scheduler: solver returned a plan for a different epoch")

// Solve sends the problem to the solver and returns its plan.
func (s *Solver) Solve(ctx context.Context, p Problem) (Plan, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return Plan{}, fmt.Errorf("scheduler: encode problem: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.path)
	cmd.Stdin = bytes.NewReader(payload)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return Plan{}, fmt.Errorf("scheduler: solver exceeded its %s deadline; "+
			"the previous plan stays in force", s.timeout)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return Plan{}, fmt.Errorf("scheduler: solver failed: %s", msg)
	}

	var plan Plan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		return Plan{}, fmt.Errorf("scheduler: parse plan: %w", err)
	}
	if plan.Epoch != p.Epoch {
		return Plan{}, fmt.Errorf("%w: sent %d, got %d", ErrStalePlan, p.Epoch, plan.Epoch)
	}
	return plan, nil
}

// Version asks the solver to identify itself, used at startup so a mismatched
// or missing binary is a clear error at boot rather than a failed tick later.
func (s *Solver) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, s.path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("scheduler: cannot run solver at %s: %w", s.path, err)
	}
	return strings.TrimSpace(string(out)), nil
}
