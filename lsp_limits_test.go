package main

import (
	"fmt"
	"testing"
	"time"
)

func TestLSPLimits(t *testing.T) {
	idle, hard := lspLimits()
	fmt.Printf("idle=%s hard=%s\n", idle, hard)
	if idle != 3*time.Minute || hard != 15*time.Minute {
		t.Fatalf("limits idle=%s hard=%s", idle, hard)
	}
	proc, err := startSleep()
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Kill()
	started := time.Now().Add(-10 * time.Millisecond)
	reason := enforceLSP(proc.Pid, started, started, time.Now(), 5*time.Millisecond, time.Hour)
	dead := processDead(proc)
	fmt.Printf("killed %s pid=%d dead=%v\n", reason, proc.Pid, dead)
	if reason != "idle-kill" || !dead {
		t.Fatalf("idle-kill reason=%s dead=%v", reason, dead)
	}
	proc, err = startSleep()
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Kill()
	started = time.Now().Add(-20 * time.Millisecond)
	reason = enforceLSP(proc.Pid, started, time.Now(), time.Now(), time.Hour, 10*time.Millisecond)
	dead = processDead(proc)
	fmt.Printf("killed %s pid=%d dead=%v\n", reason, proc.Pid, dead)
	if reason != "hard-stop" || !dead {
		t.Fatalf("hard-stop reason=%s dead=%v", reason, dead)
	}
}
