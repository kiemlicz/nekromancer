package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// PiKVM drives the host through the ATX API, or the Switch API when Port is set.
type PiKVM struct {
	HTTP               *http.Client
	URL                string
	Username, Password string
	// Port is the name of the host's Switch port, looked up on every call so renumbering is harmless.
	Port string
}

func (p *PiKVM) request(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.URL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-KVMD-User", p.Username)
	req.Header.Set("X-KVMD-Passwd", p.Password)
	return do(p.HTTP, req)
}

func (p *PiKVM) press(ctx context.Context, action string) error {
	path := "/api/atx/power?action=" + action
	if p.Port != "" {
		index, _, err := p.switchPort(ctx)
		if err != nil {
			return err
		}
		path = fmt.Sprintf("/api/switch/atx/power?port=%d&action=%s", index, action)
	}
	_, err := p.request(ctx, http.MethodPost, path)
	return err
}

// switchPort finds the port by exact name. A press on a guessed port powers off the wrong machine,
// so anything but one match is an error.
func (p *PiKVM) switchPort(ctx context.Context) (index int, powered bool, err error) {
	body, err := p.request(ctx, http.MethodGet, "/api/switch")
	if err != nil {
		return 0, false, err
	}
	var state struct {
		Result struct {
			Model struct {
				Ports []struct {
					Name string `json:"name"`
				} `json:"ports"`
			} `json:"model"`
			ATX struct {
				LEDs struct {
					Power []bool `json:"power"`
				} `json:"leds"`
			} `json:"atx"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return 0, false, fmt.Errorf("decoding Switch state: %w", err)
	}
	index = -1
	for i, port := range state.Result.Model.Ports {
		if port.Name != p.Port {
			continue
		}
		if index >= 0 {
			return 0, false, fmt.Errorf("Switch ports %d and %d are both named %q", index, i, p.Port)
		}
		index = i
	}
	if index < 0 {
		return 0, false, fmt.Errorf("no Switch port named %q", p.Port)
	}
	leds := state.Result.ATX.LEDs.Power
	if index >= len(leds) {
		return 0, false, fmt.Errorf("no power LED for Switch port %q", p.Port)
	}
	return index, leds[index], nil
}

// PowerOn checks the LED first: NotReady hosts get woken and may be running,
// and a click would shut them down.
func (p *PiKVM) PowerOn(ctx context.Context) error {
	off, err := p.Offline(ctx)
	if err != nil {
		return err
	}
	if !off {
		return nil
	}
	return p.press(ctx, "on")
}

// PowerOff checks the LED first: "off" emulates a button click, which must
// not reach a host that is already down.
func (p *PiKVM) PowerOff(ctx context.Context) error {
	off, err := p.Offline(ctx)
	if err != nil {
		return err
	}
	if off {
		return nil
	}
	return p.press(ctx, "off")
}

func (p *PiKVM) Offline(ctx context.Context) (bool, error) {
	if p.Port != "" {
		_, powered, err := p.switchPort(ctx)
		return !powered && err == nil, err
	}
	body, err := p.request(ctx, http.MethodGet, "/api/atx")
	if err != nil {
		return false, err
	}
	var state struct {
		Result struct {
			Enabled bool `json:"enabled"`
			LEDs    struct {
				Power bool `json:"power"`
			} `json:"leds"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return false, fmt.Errorf("decoding ATX state: %w", err)
	}
	if !state.Result.Enabled {
		return false, errors.New("ATX is disabled on this PiKVM, power state unknown")
	}
	return !state.Result.LEDs.Power, nil
}
