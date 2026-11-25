package main

import (
	"os"
	"syscall"
	"time"
)

const (
	lspIdleKill = 3 * time.Minute
	lspHardStop = 15 * time.Minute
)

func enforceLSP(pid int, started, last, now time.Time, idle, hard time.Duration) string {
	reason := ""
	if !started.IsZero() && now.Sub(started) >= hard {
		reason = "hard-stop"
	} else if !last.IsZero() && now.Sub(last) >= idle {
		reason = "idle-kill"
	}
	if reason == "" {
		return ""
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return reason
}

func lspLimits() (time.Duration, time.Duration) {
	return lspIdleKill, lspHardStop
}

func processDead(proc *os.Process) bool {
	done := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

func startSleep() (*os.Process, error) {
	proc, err := os.StartProcess("/bin/sleep", []string{"sleep", "30"}, &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	return proc, err
}
