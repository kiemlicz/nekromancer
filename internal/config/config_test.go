package config

import (
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
  provider: mikrotik
  mac: "AA:BB:CC:DD:EE:01"
  interface: bridge
  address: 192.168.1.50
- name: mini2
  provider: pikvm
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
	if c.NotReadyGrace.Duration != time.Minute || c.WakeCooldown.Duration != 5*time.Minute || c.PowerOffRetry.Duration != 2*time.Minute {
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

func TestParseRejects(t *testing.T) {
	mikrotikHost := func(mutate string) string {
		return "mikrotik: {url: https://r}\nhosts:\n- {name: a, provider: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: 10.0.0.1" + mutate + "}\n"
	}
	for name, tc := range map[string]struct{ in, want string }{
		"unknown field":    {"bogus: 1\n" + valid, "bogus"},
		"no hosts":         {"mikrotik: {url: https://r}\n", "no hosts"},
		"missing name":     {"hosts:\n- {provider: pikvm, pikvmURL: https://p}\n", "name"},
		"duplicate":        {"hosts:\n- {name: a, provider: pikvm, pikvmURL: https://p}\n- {name: a, provider: pikvm, pikvmURL: https://p}\n", "duplicate"},
		"unknown provider": {"hosts:\n- {name: a, provider: ipmi}\n", "provider"},
		"bad mac":          {"mikrotik: {url: https://r}\nhosts:\n- {name: a, provider: mikrotik, mac: nope, interface: b, address: 10.0.0.1}\n", "mac"},
		"no interface":     {"mikrotik: {url: https://r}\nhosts:\n- {name: a, provider: mikrotik, mac: 'AA:BB:CC:DD:EE:01', address: 10.0.0.1}\n", "interface"},
		"bad address":      {"mikrotik: {url: https://r}\nhosts:\n- {name: a, provider: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: mini}\n", "address"},
		"no mikrotik url":  {"hosts:\n- {name: a, provider: mikrotik, mac: 'AA:BB:CC:DD:EE:01', interface: b, address: 10.0.0.1}\n", "mikrotik.url"},
		"no pikvm url":     {"hosts:\n- {name: a, provider: pikvm}\n", "pikvmURL"},
		"negative timeout": {"defaultIdleTimeout: -1m\n" + mikrotikHost(""), "defaultIdleTimeout"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
