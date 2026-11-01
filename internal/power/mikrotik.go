package power

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Mikrotik wakes hosts through a RouterOS WoL call.
type Mikrotik struct {
	HTTP               *http.Client
	URL                string
	Username, Password string
	MAC, Interface     string
}

func (m *Mikrotik) PowerOn(ctx context.Context) error {
	payload, err := json.Marshal(map[string]string{"mac": m.MAC, "interface": m.Interface})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.URL, "/")+"/rest/tool/wol", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(m.Username, m.Password)
	_, err = do(m.HTTP, req)
	return err
}
