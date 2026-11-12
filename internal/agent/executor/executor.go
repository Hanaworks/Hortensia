// Package executor owns real subprocess facts, independent of network sessions.
package executor

import (
	"context"
	"docker-manager/internal/agent/ledger"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrUnsupported = errors.New("execution_platform_unsupported")
var ErrCommand = errors.New("command_invalid")
var ErrBudget = errors.New("command_output_budget_exceeded")
var ErrExecution = errors.New("command_failed")

type Command struct {
	Program string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

func validate(c Command) error {
	if !filepath.IsAbs(c.Program) || !filepath.IsAbs(c.Dir) || c.Timeout < time.Second || c.Timeout > 24*time.Hour || strings.ContainsRune(c.Program, 0) {
		return ErrCommand
	}
	for _, s := range append(append([]string{}, c.Args...), c.Env...) {
		if strings.ContainsRune(s, 0) {
			return ErrCommand
		}
	}
	return nil
}

type Process struct {
	cmd     *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	reason  string
	outcome ledger.ProcessOutcome
	output  outputCollector
}

func Start(ctx context.Context, c Command) (*Process, error) {
	if err := validate(c); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(c.Program, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{}, c.Env...)
	if err := configureProcess(cmd, true); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = streamWriter{&p.output, 0}
	cmd.Stderr = streamWriter{&p.output, 1}
	if err := cmd.Start(); err != nil {
		return nil, ErrExecution
	}
	go p.reap()
	go func() {
		timer := time.NewTimer(c.Timeout)
		defer timer.Stop()
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			p.Interrupt("shutdown")
		case <-timer.C:
			p.Interrupt("deadline")
		}
	}()
	return p, nil
}
func (p *Process) PID() int { return p.cmd.Process.Pid }
func (p *Process) Interrupt(reason string) {
	if reason == "cancel" {
		reason = "canceled"
	}
	if reason == "shutdown" {
		reason = "agent_stopping"
	}
	if reason != "canceled" && reason != "deadline" && reason != "agent_stopping" && reason != "signal" {
		reason = "canceled"
	}
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return
	default:
	}
	if p.reason != "" {
		p.mu.Unlock()
		return
	}
	p.reason = reason
	terminateProcess(p.cmd, false)
	p.mu.Unlock()
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-p.done:
			return
		case <-timer.C:
			terminateProcess(p.cmd, true)
		}
	}()
}
func (p *Process) reap() {
	err := p.cmd.Wait()
	output, truncated := p.output.finish()
	p.mu.Lock()
	defer p.mu.Unlock()
	o := ledger.ProcessOutcome{Outcome: "unknown", FinishedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Output: output, OutputTruncated: truncated}
	if st := p.cmd.ProcessState; st != nil && st.Exited() {
		code := st.ExitCode()
		o.ExitCode = &code
		o.Outcome = "normal_exit"
	}
	if p.reason != "" || wasSignaled(p.cmd) {
		o.Outcome = "interrupted"
		o.InterruptReason = p.reason
		if o.InterruptReason == "" {
			o.InterruptReason = "signal"
		}
		// A parent can exit while a descendant closes its inherited pipes but
		// remains alive. Finish interrupted ownership of the whole group.
		_ = terminateProcess(p.cmd, true)
	}
	if o.Outcome == "unknown" || err != nil && o.ExitCode == nil && o.Outcome != "interrupted" {
		o.ErrorCode = "process_wait_failed"
	}
	p.outcome = o
	close(p.done)
}
func (p *Process) Wait() ledger.ProcessOutcome {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	o := p.outcome
	o.Output = append([]byte{}, o.Output...)
	return o
}

type boundedBuffer struct {
	mu       sync.Mutex
	b        []byte
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - len(b.b)
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.b = append(b.b, p...)
	return n, nil
}

// RunReadOnly is the read-command boundary. Its caller fixes the CLI verbs.
// Unlike Start it is available for controlled Windows CLI fixture tests.
func RunReadOnly(ctx context.Context, c Command, maxBytes int) ([]byte, error) {
	if err := validate(c); err != nil {
		return nil, err
	}
	if maxBytes < 1 || maxBytes > 8<<20 {
		return nil, ErrCommand
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Program, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{}, c.Env...)
	if err := configureProcess(cmd, false); err != nil {
		return nil, err
	}
	cmd.Cancel = func() error { return terminateProcess(cmd, true) }
	cmd.WaitDelay = 2 * time.Second
	out := &boundedBuffer{limit: maxBytes}
	stderr := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if out.overflow || stderr.overflow {
		return nil, ErrBudget
	}
	if err != nil {
		return nil, ErrExecution
	}
	return out.b, nil
}
