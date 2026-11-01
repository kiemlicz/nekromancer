// Package config loads the static hardware inventory.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	WakeMikrotik = "mikrotik"
	WakePiKVM    = "pikvm"

	// ShutdownOS has the node-agent run the shutdown command on the host.
	ShutdownOS    = "os"
	ShutdownPiKVM = "pikvm"
)

type Config struct {
	DefaultIdleTimeout metav1.Duration `json:"defaultIdleTimeout"`
	// WakeCooldown is how long a woken host gets to register before another attempt.
	WakeCooldown metav1.Duration `json:"wakeCooldown"`
	// PowerOffRetry is how long a host may stay online before power off is requested again.
	PowerOffRetry metav1.Duration `json:"powerOffRetry"`
	Mikrotik      Mikrotik        `json:"mikrotik"`
	PiKVM         TLS             `json:"pikvm"`
	Hosts         []Host          `json:"hosts"`
}

type TLS struct {
	InsecureSkipVerify bool   `json:"insecureSkipVerify"`
	CAFile             string `json:"caFile"`
}

type Mikrotik struct {
	URL string `json:"url"`
	TLS `json:",inline"`
}

// Host describes one physical machine. Name must equal its Node name.
type Host struct {
	Name string `json:"name"`
	// Labels must mirror the kubelet --node-labels, pending pods are matched against them.
	Labels map[string]string `json:"labels"`
	Wake   string            `json:"wake"`
	// Shutdown defaults to ShutdownOS.
	Shutdown  string `json:"shutdown"`
	MAC       string `json:"mac"`
	Interface string `json:"interface"`
	Address   string `json:"address"`
	// PiKVMURL identifies the machine wired to that PiKVM's ATX header, or with PiKVMPort, the PiKVM it's behind.
	PiKVMURL string `json:"pikvmURL"`
	// PiKVMPort is the name of the host's Switch port on that PiKVM, by default the host name.
	// An explicit empty value selects the plain ATX API, for a PiKVM without a Switch.
	PiKVMPort *string `json:"pikvmPort"`
}

// Port returns the Switch port name, empty for the plain ATX API.
func (h Host) Port() string {
	if h.PiKVMPort == nil {
		return ""
	}
	return *h.PiKVMPort
}

// UsesPiKVM tells whether the host is wired to a PiKVM, whose power LED then proves its state.
func (h Host) UsesPiKVM() bool { return h.Wake == WakePiKVM || h.Shutdown == ShutdownPiKVM }

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	for _, d := range []struct {
		name string
		v    *metav1.Duration
		def  time.Duration
	}{
		{"defaultIdleTimeout", &c.DefaultIdleTimeout, 10 * time.Minute},
		{"wakeCooldown", &c.WakeCooldown, 5 * time.Minute},
		{"powerOffRetry", &c.PowerOffRetry, 2 * time.Minute},
	} {
		if d.v.Duration < 0 {
			return nil, fmt.Errorf("%s must not be negative", d.name)
		}
		if d.v.Duration == 0 {
			d.v.Duration = d.def
		}
	}
	for i := range c.Hosts {
		if c.Hosts[i].Shutdown == "" {
			c.Hosts[i].Shutdown = ShutdownOS
		}
		if c.Hosts[i].UsesPiKVM() && c.Hosts[i].PiKVMPort == nil {
			c.Hosts[i].PiKVMPort = &c.Hosts[i].Name
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Hosts) == 0 {
		return errors.New("no hosts configured")
	}
	seen := map[string]bool{}
	pikvms := map[string]map[string]string{}
	for i, h := range c.Hosts {
		if h.Name == "" {
			return fmt.Errorf("hosts[%d]: name is required", i)
		}
		if seen[h.Name] {
			return fmt.Errorf("hosts[%d]: duplicate name %q", i, h.Name)
		}
		seen[h.Name] = true
		if err := c.validateProvider(h); err != nil {
			return fmt.Errorf("host %q: %w", h.Name, err)
		}
		if !h.UsesPiKVM() {
			continue
		}
		// The plain ATX API has no target host: sharing a URL would press the wrong machine's button.
		key := strings.ToLower(strings.TrimRight(h.PiKVMURL, "/"))
		for port, other := range pikvms[key] {
			if port == "" || h.Port() == "" {
				return fmt.Errorf("hosts %q and %q have the same pikvmURL, so both need a pikvmPort", other, h.Name)
			}
			if port == h.Port() {
				return fmt.Errorf("hosts %q and %q have the same pikvmPort %q", other, h.Name, port)
			}
		}
		if pikvms[key] == nil {
			pikvms[key] = map[string]string{}
		}
		pikvms[key][h.Port()] = h.Name
	}
	return nil
}

func (c *Config) validateProvider(h Host) error {
	switch h.Wake {
	case WakeMikrotik:
		if !validURL(c.Mikrotik.URL) {
			return errors.New("mikrotik.url must be set to an absolute URL")
		}
		if _, err := net.ParseMAC(h.MAC); err != nil {
			return fmt.Errorf("invalid mac: %w", err)
		}
		if h.Interface == "" {
			return errors.New("interface is required")
		}
	case WakePiKVM:
	default:
		return fmt.Errorf("unknown wake provider %q", h.Wake)
	}
	if h.Shutdown != ShutdownOS && h.Shutdown != ShutdownPiKVM {
		return fmt.Errorf("unknown shutdown provider %q", h.Shutdown)
	}
	if h.UsesPiKVM() {
		if !validURL(h.PiKVMURL) {
			return errors.New("pikvmURL must be set to an absolute URL")
		}
		return nil
	}
	if h.PiKVMPort != nil {
		return errors.New("pikvmPort needs a pikvm wake or shutdown")
	}
	// Without a PiKVM, ICMP is the only way to confirm the host is down.
	if ip := net.ParseIP(h.Address); ip == nil || ip.To4() == nil {
		return fmt.Errorf("address %q is not an IPv4 address", h.Address)
	}
	return nil
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}
