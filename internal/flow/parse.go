package flow

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

// Parse reads and unmarshals a flow definition from a file.
func Parse(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read flow: %w", err)
	}
	return ParseBytes(data)
}

// ParseBytes unmarshals a flow with strict decoding: unknown keys and duplicate
// keys are errors, so a typo'd field fails loudly instead of being ignored.
func ParseBytes(data []byte) (*Flow, error) {
	var f Flow
	if err := yaml.UnmarshalWithOptions(data, &f, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse flow: %w", err)
	}
	return &f, nil
}
