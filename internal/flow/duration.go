package flow

import (
	"time"

	"github.com/goccy/go-yaml"
)

// Duration is a time.Duration that unmarshals from a YAML string like "5m" or
// "2s". A bare number is rejected — durations must carry a unit, removing the
// "5 what?" ambiguity.
type Duration time.Duration

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements goccy's BytesUnmarshaler.
func (d *Duration) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
