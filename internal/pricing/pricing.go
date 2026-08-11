// Package pricing loads accelerator hourly rates.
//
// The rates that ship with ullage are a starting point, not an authority: real
// prices depend on region, commitment, reservation and negotiated discount, and
// anyone using these numbers to justify a decision should replace them with
// their own. That is why the source is always printed alongside the money.
package pricing

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/ullage-project/ullage/pkg/ullage/api"
	"gopkg.in/yaml.v3"
)

//go:embed default.yaml
var defaultYAML []byte

type file struct {
	Source        string             `yaml:"source"`
	Currency      string             `yaml:"currency"`
	PerGPUHour    float64            `yaml:"perGPUHour"`
	PerSKUGPUHour map[string]float64 `yaml:"perSKUGPUHour"`
}

// Load reads a pricing file, or returns the built-in defaults when path is
// empty.
func Load(path string) (*api.Pricing, error) {
	data := defaultYAML
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading pricing file: %w", err)
		}
		data = b
	}
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing pricing file: %w", err)
	}
	if f.Currency == "" {
		f.Currency = "USD"
	}
	if f.Source == "" && path != "" {
		f.Source = path
	}
	return &api.Pricing{
		Source:        f.Source,
		Currency:      f.Currency,
		PerGPUHour:    f.PerGPUHour,
		PerSKUGPUHour: f.PerSKUGPUHour,
	}, nil
}
