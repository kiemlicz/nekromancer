package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeCommand records its arguments instead of touching the host.
func fakeCommand(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "cmd")
	script := "#!/bin/sh\necho \"$@\" > " + record + "\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func TestCommandRunsArgv(t *testing.T) {
	bin, record := fakeCommand(t, "exit 0")
	if err := (Command{Argv: []string{bin, "-h", "now"}}).PowerOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "-h now" {
		t.Fatalf("args = %q", got)
	}
}

func TestCommandFailureCarriesOutput(t *testing.T) {
	bin, _ := fakeCommand(t, "echo nope >&2; exit 3")
	err := (Command{Argv: []string{bin}}).PowerOff(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostShutdownIsTheRegularCommandInHostNamespaces(t *testing.T) {
	got := HostShutdown.Argv
	if got[0] != "/nsenter" || !slices.Equal(got[len(got)-3:], []string{"shutdown", "-h", "now"}) {
		t.Fatalf("argv = %v", got)
	}
	for _, flag := range []string{"-t", "-m", "-p"} {
		if !slices.Contains(got, flag) {
			t.Errorf("argv %v misses %s", got, flag)
		}
	}
}
