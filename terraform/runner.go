package terraform

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Runner runs the Terraform binary. The real one is execRunner; tests inject a fake.
type Runner interface {
	// Run executes binary with args in dir and exactly the environment env (no inherited
	// variables, no shell). exit is the process exit code; err is set only when the process
	// could not run or was killed.
	Run(ctx context.Context, dir string, env []string, binary string, args ...string) (stdout, stderr []byte, exit int, err error)
}

// execRunner runs the binary as a child process.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, env []string, binary string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &limitedWriter{buf: &errb, max: 1 << 20}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return out.Bytes(), errb.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return out.Bytes(), errb.Bytes(), -1, err
	}
	return out.Bytes(), errb.Bytes(), 0, nil
}

// limitedWriter keeps at most max bytes: stderr of a long apply can be large.
type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}
