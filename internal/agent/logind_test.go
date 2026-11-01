package agent

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeLogind records calls. Nothing here can reach the real system bus.
type fakeLogind struct{ calls chan bool }

func (f fakeLogind) PowerOff(interactive bool) *dbus.Error {
	f.calls <- interactive
	return nil
}

func privateBus(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	addr := "unix:path=" + filepath.Join(t.TempDir(), "bus")
	cmd := exec.Command(bin, "--session", "--nofork", "--print-address=1", "--address="+addr)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

func TestLogindPowerOffCallsManager(t *testing.T) {
	addr := privateBus(t)
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fake := fakeLogind{calls: make(chan bool, 1)}
	if err := conn.Export(fake, "/org/freedesktop/login1", "org.freedesktop.login1.Manager"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RequestName("org.freedesktop.login1", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (Logind{Address: addr}).PowerOff(ctx); err != nil {
		t.Fatal(err)
	}
	if interactive := <-fake.calls; interactive {
		t.Fatal("must not be interactive")
	}
}

func TestLogindPowerOffFailsWithoutService(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (Logind{Address: privateBus(t)}).PowerOff(ctx); err == nil {
		t.Fatal("want error")
	}
}
