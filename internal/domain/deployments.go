package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Deployments contains protocol addresses for one chain. An absent address
// disables that deployment; callers must not substitute another chain's value.
type Deployments struct {
	Factories     map[string]Address
	V4PoolManager Address
	V4StateView   Address
}

type deploymentJSON struct {
	Factories     map[string]string `json:"factories,omitempty"`
	V4PoolManager string            `json:"v4_pool_manager,omitempty"`
	V4StateView   string            `json:"v4_state_view,omitempty"`
}

func (d Deployments) MarshalJSON() ([]byte, error) {
	raw := deploymentJSON{Factories: make(map[string]string, len(d.Factories))}
	for name, address := range d.Factories {
		raw.Factories[name] = address.Hex()
	}
	if d.V4PoolManager != (Address{}) {
		raw.V4PoolManager = d.V4PoolManager.Hex()
	}
	if d.V4StateView != (Address{}) {
		raw.V4StateView = d.V4StateView.Hex()
	}
	return json.Marshal(raw)
}

func (d *Deployments) UnmarshalJSON(data []byte) error {
	var raw deploymentJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	parsed := Deployments{Factories: make(map[string]Address, len(raw.Factories))}
	for name, text := range raw.Factories {
		address, err := ParseAddress(text)
		if err != nil {
			return fmt.Errorf("factory %s: %w", name, err)
		}
		parsed.Factories[name] = address
	}
	for _, entry := range []struct {
		text   string
		target *Address
	}{{raw.V4PoolManager, &parsed.V4PoolManager}, {raw.V4StateView, &parsed.V4StateView}} {
		if entry.text == "" {
			continue
		}
		address, err := ParseAddress(entry.text)
		if err != nil {
			return err
		}
		*entry.target = address
	}
	*d = parsed
	return nil
}
