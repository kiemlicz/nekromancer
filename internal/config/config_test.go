package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
mikrotik:
  url: https://router
hosts:
- name: mini1
  labels: {workload: plex}
  wake: mikrotik
  mac: "AA:BB:CC:DD:EE:01"
  interface: bridge
  address: 192.168.1.50
- name: mini2
  wake: pikvm
  shutdown: pikvm
  pikvmURL: https://pikvm
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultIdleTimeout.Duration != 10*time.Minute {
		t.Errorf("idle timeout = %v", c.DefaultIdleTimeout.Duration)
	}
	if c.WakeCooldown.Duration != 5*time.Minute || c.PowerOffRetry.Duration != 2*time.Minute {
		t.Errorf("defaults = %+v", c)
	}
	if c.Hosts[0].Labels["workload"] != "plex" {
		t.Errorf("labels = %v", c.Hosts[0].Labels)
	}
}

func TestParseKeepsExplicitDurations(t *testing.T) {
	c, err := Parse([]byte("defaultIdleTimeout: 30m\n" + valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultIdleTimeout.Duration != 30*time.Minute {
		t.Errorf("idle timeout = %v", c.DefaultIdleTimeout.Duration)
	}
}

func mikrotikHostWith(extra string) string {
	return "mikrotik: {url: https://r}\nhosts:\n- {name: a, wake: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: 10.0.0.1" + extra + "}\n"
}

func TestShutdownDefaultsToOS(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Hosts[0].Shutdown != ShutdownOS {
		t.Errorf("mini1 shutdown = %q", c.Hosts[0].Shutdown)
	}
	if c.Hosts[1].Shutdown != ShutdownPiKVM {
		t.Errorf("mini2 shutdown = %q", c.Hosts[1].Shutdown)
	}
}

// ATX wiring proves the LED is readable, so these hosts need no address.
func TestParseAcceptsMixedProviders(t *testing.T) {
	for name, host := range map[string]string{
		"wol wake, atx shutdown": "{name: a, wake: mikrotik, shutdown: pikvm, mac: 'AA:BB:CC:DD:EE:01', interface: b, pikvmURL: https://p}",
		"atx wake, os shutdown":  "{name: a, wake: pikvm, shutdown: os, pikvmURL: https://p}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte("mikrotik: {url: https://r}\nhosts:\n- " + host + "\n")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unknown field":                 {"bogus: 1\n" + valid, "bogus"},
		"no hosts":                      {"mikrotik: {url: https://r}\n", "no hosts"},
		"missing name":                  {"hosts:\n- {wake: pikvm, pikvmURL: https://p}\n", "name"},
		"duplicate":                     {"hosts:\n- {name: a, wake: pikvm, pikvmURL: https://p}\n- {name: a, wake: pikvm, pikvmURL: https://p}\n", "duplicate"},
		"unknown wake":                  {"hosts:\n- {name: a, wake: ipmi}\n", "wake"},
		"unknown shutdown":              {mikrotikHostWith(", shutdown: ipmi"), "shutdown"},
		"old provider key":              {"hosts:\n- {name: a, provider: pikvm, pikvmURL: https://p}\n", "provider"},
		"mikrotik cannot shut down":     {mikrotikHostWith(", shutdown: mikrotik"), "shutdown"},
		"bad mac":                       {"mikrotik: {url: https://r}\nhosts:\n- {name: a, wake: mikrotik, mac: nope, interface: b, address: 10.0.0.1}\n", "mac"},
		"no interface":                  {"mikrotik: {url: https://r}\nhosts:\n- {name: a, wake: mikrotik, mac: 'AA:BB:CC:DD:EE:01', address: 10.0.0.1}\n", "interface"},
		"bad address":                   {"mikrotik: {url: https://r}\nhosts:\n- {name: a, wake: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: mini}\n", "address"},
		"no mikrotik url":               {"hosts:\n- {name: a, wake: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: 10.0.0.1}\n", "mikrotik.url"},
		"no pikvm url":                  {"hosts:\n- {name: a, wake: pikvm}\n", "pikvmURL"},
		"pikvm shutdown without url":    {mikrotikHostWith(", shutdown: pikvm"), "pikvmURL"},
		"shared pikvm":                  {"hosts:\n- {name: a, wake: pikvm, pikvmURL: 'https://p/'}\n- {name: b, wake: pikvm, pikvmURL: 'https://P'}\n", "same pikvmURL"},
		"shared pikvm, one port":        {"hosts:\n- {name: a, wake: pikvm, pikvmURL: 'https://p', pikvmPort: a}\n- {name: b, wake: pikvm, pikvmURL: 'https://p'}\n", "same pikvmURL"},
		"shared pikvm, one port first":  {"hosts:\n- {name: a, wake: pikvm, pikvmURL: 'https://p'}\n- {name: b, wake: pikvm, pikvmURL: 'https://p', pikvmPort: b}\n", "same pikvmURL"},
		"shared pikvm port":             {"hosts:\n- {name: a, wake: pikvm, pikvmURL: 'https://p', pikvmPort: x}\n- {name: b, wake: pikvm, pikvmURL: 'https://P/', pikvmPort: x}\n", "same pikvmPort"},
		"port without pikvm":            {mikrotikHostWith(", pikvmPort: x"), "pikvmPort"},
		"os shutdown without any probe": {"mikrotik: {url: https://r}\nhosts:\n- {name: a, wake: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b}\n", "address"},
		"negative timeout":              {"defaultIdleTimeout: -1m\n" + mikrotikHostWith(""), "defaultIdleTimeout"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestHostsShareAPiKVMThroughPorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "hosts:\n- {name: a, wake: pikvm, pikvmURL: 'https://p/', pikvmPort: a}\n- {name: b, wake: pikvm, shutdown: pikvm, pikvmURL: 'https://P', pikvmPort: b}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
