// Package describe is the answer every plugin gives to describe: who it is, what it can do, the
// settings it reads, what it needs, and the versions of each target (the host it runs in) it
// holds for, as one flat JSON object its kind's own keys share.
package describe

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/sideways-works/plugkit/semver"
)

var (
	// ErrNoRange is a plugin that declares no range for the target asking.
	ErrNoRange = errors.New("no range declared")
	// ErrUnsupported is a plugin whose ranges exclude the version at hand.
	ErrUnsupported = errors.New("unsupported version")
	// ErrKey is a key the envelope, a target and a payload would write twice.
	ErrKey = errors.New("a key written twice")
)

// Description is the envelope. Targets are the plugin's base range per target, each written as
// a key of its own beside the envelope's ("host": ">=1.0 <2").
type Description struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Version      string          `json:"version"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Settings     json.RawMessage `json:"settings,omitempty"`
	Needs        []string        `json:"needs,omitempty"`

	Targets map[string]string `json:"-"`

	// rest holds the keys a decoded object had besides the envelope's, where a target's range is
	// found when Targets doesn't name it.
	rest map[string]json.RawMessage
}

var envelopeKeys = []string{"name", "kind", "version", "capabilities", "settings", "needs"}

// Capability is a plain name in JSON, or {"name": ..., "<target>": "<range>"} for one that holds
// only for those versions of a target.
type Capability struct {
	Name   string
	Ranges map[string]string
}

func (c Capability) MarshalJSON() ([]byte, error) {
	if len(c.Ranges) == 0 {
		return json.Marshal(c.Name) //nolint:wrapcheck // a string
	}

	if _, ok := c.Ranges["name"]; ok {
		return nil, fmt.Errorf("capability %s: %w: a target can't be called name", c.Name, ErrKey)
	}

	m := map[string]string{"name": c.Name}
	maps.Copy(m, c.Ranges)

	return json.Marshal(m) //nolint:wrapcheck // a map of strings
}

func (c *Capability) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &c.Name); err == nil {
		c.Ranges = nil

		return nil
	}

	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("capability: a name, or an object of name and ranges: %w", err)
	}

	c.Name = m["name"]
	delete(m, "name")
	c.Ranges = m

	return nil
}

type envelope struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Version      string          `json:"version"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Settings     json.RawMessage `json:"settings,omitempty"`
	Needs        []string        `json:"needs,omitempty"`
}

func (d Description) fields() (map[string]json.RawMessage, error) {
	data, err := json.Marshal(envelope{d.Name, d.Kind, d.Version, d.Capabilities, d.Settings, d.Needs})
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", d.Name, err)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("describe %s: %w", d.Name, err)
	}

	for target, rng := range d.Targets {
		if slices.Contains(envelopeKeys, target) {
			return nil, fmt.Errorf("describe %s: %w: target %q is an envelope key", d.Name, ErrKey, target)
		}

		value, err := json.Marshal(rng)
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", d.Name, err)
		}

		out[target] = value
	}

	return out, nil
}

func (d Description) MarshalJSON() ([]byte, error) {
	f, err := d.fields()
	if err != nil {
		return nil, err
	}

	return json.Marshal(f) //nolint:wrapcheck // a map of raw values
}

func (d *Description) UnmarshalJSON(data []byte) error {
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("describe: %w", err)
	}

	for _, k := range envelopeKeys {
		delete(all, k)
	}

	*d = Description{
		Name: e.Name, Kind: e.Kind, Version: e.Version, Capabilities: e.Capabilities, Settings: e.Settings, Needs: e.Needs,
		rest: all,
	}

	return nil
}

// Range is the plugin's base range for target.
func (d Description) Range(target string) (semver.Range, error) {
	text, ok := d.Targets[target]
	if !ok {
		raw, found := d.rest[target]
		if !found || json.Unmarshal(raw, &text) != nil {
			return semver.Range{}, fmt.Errorf("%w: %s %s declares no %s range", ErrNoRange, d.Name, d.Version, target)
		}
	}

	r, err := semver.ParseRange(text)
	if err != nil {
		return semver.Range{}, fmt.Errorf("%s %s: %w", d.Name, d.Version, err)
	}

	return r, nil
}

// Check refuses a plugin whose base range for target leaves out the target's version.
func (d Description) Check(target string, version semver.Version) error {
	r, err := d.Range(target)
	if err != nil {
		return err
	}

	if !r.Contains(version) {
		return fmt.Errorf("%w: %s %s supports %s %s; this is %s", ErrUnsupported, d.Name, d.Version, target, r, version)
	}

	return nil
}

// Has says whether the plugin declares the capability for this version of target: one whose
// range for the target leaves the version out, or can't be read, counts as absent.
func (d Description) Has(capability, target string, version semver.Version) bool {
	for _, c := range d.Capabilities {
		if c.Name != capability {
			continue
		}

		text, ok := c.Ranges[target]
		if !ok {
			return true
		}

		r, err := semver.ParseRange(text)

		return err == nil && r.Contains(version)
	}

	return false
}

// Accepts checks the plugin's own version against a range the consumer sets for it: a match
// needs both the plugin's range for the target and the consumer's for the plugin to hold.
func (d Description) Accepts(accepts semver.Range) error {
	v, err := semver.Parse(d.Version)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Name, err)
	}

	if !accepts.Contains(v) {
		return fmt.Errorf("%w: %s %s is outside the accepted %s", ErrUnsupported, d.Name, d.Version, accepts)
	}

	return nil
}

// Of is a description with its kind's payload, whose keys sit beside the envelope's in the same
// object. Payload may be a struct of the kind's keys, or json.RawMessage for all the others.
// A kind's struct must not embed Description instead: its JSON methods would encode only the
// envelope.
type Of[T any] struct {
	Description

	Payload T
}

func (d Of[T]) MarshalJSON() ([]byte, error) {
	out, err := d.fields()
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(d.Payload)
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", d.Name, err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("describe %s: the payload is no object: %w", d.Name, err)
	}

	for k, v := range payload {
		if _, taken := out[k]; taken {
			return nil, fmt.Errorf("describe %s: %w: the payload's %q is the envelope's or a target's", d.Name, ErrKey, k)
		}

		out[k] = v
	}

	return json.Marshal(out) //nolint:wrapcheck // a map of raw values
}

func (d *Of[T]) UnmarshalJSON(data []byte) error {
	if err := d.Description.UnmarshalJSON(data); err != nil {
		return err
	}

	rest, err := json.Marshal(d.rest)
	if err != nil {
		return fmt.Errorf("describe %s: %w", d.Name, err)
	}

	var payload T
	if err := json.Unmarshal(rest, &payload); err != nil {
		return fmt.Errorf("describe %s: %w", d.Name, err)
	}

	d.Payload = payload

	return nil
}
