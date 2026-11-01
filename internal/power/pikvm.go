package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// PiKVM drives the host through the ATX API.
type PiKVM struct {
	HTTP               *http.Client
	URL                string
	Username, Password string
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
	_, err := p.request(ctx, http.MethodPost, "/api/atx/power?action="+action)
	return err
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
