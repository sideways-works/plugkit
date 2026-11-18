package describe

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sideways-works/plugkit/semver"
)

type step struct {
	Images []string `json:"images,omitempty"`
}

const answer = `{"name":"image-step","kind":"step","version":"v0.4.0",` +
	`"capabilities":["items",{"name":"streams","host":">=1.3"}],` +
	`"settings":{"type":"object"},"needs":["docker"],"host":">=1.0 <2","images":["alpine:3"]}`

func version(t *testing.T, s string) semver.Version {
	t.Helper()

	v, err := semver.Parse(s)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func TestADescriptionIsOneFlatObject(t *testing.T) {
	var d Of[step]
	if err := json.Unmarshal([]byte(answer), &d); err != nil {
		t.Fatal(err)
	}

	if d.Name != "image-step" || len(d.Capabilities) != 2 || d.Capabilities[1].Ranges["host"] != ">=1.3" ||
		len(d.Payload.Images) != 1 || string(d.Settings) != `{"type":"object"}` {
		t.Fatalf("decoded %+v", d)
	}

	d.Targets = map[string]string{"host": ">=1.0 <2"}

	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}

	var a, b map[string]any
	if json.Unmarshal(data, &a) != nil || json.Unmarshal([]byte(answer), &b) != nil || !jsonEqual(a, b) {
		t.Fatalf("encoded %s", data)
	}
}

func jsonEqual(a, b any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)

	return errX == nil && errY == nil && string(x) == string(y)
}

func TestARawPayloadKeepsTheKindsKeys(t *testing.T) {
	var d Of[json.RawMessage]
	if err := json.Unmarshal([]byte(answer), &d); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(d.Payload), `"name"`) || !strings.Contains(string(d.Payload), `"images"`) {
		t.Fatalf("payload %s", d.Payload)
	}

	if _, err := json.Marshal(d); err != nil {
		t.Fatalf("a decoded description encodes again: %v", err)
	}
}

func TestCheckAndCapabilities(t *testing.T) {
	var d Description
	if err := json.Unmarshal([]byte(answer), &d); err != nil {
		t.Fatal(err)
	}

	if err := d.Check("host", version(t, "1.2.0")); err != nil {
		t.Fatal(err)
	}

	err := d.Check("host", version(t, "2.0.0"))
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "image-step v0.4.0 supports host >=1.0 <2; this is 2.0.0") {
		t.Fatalf("outside the range: %v", err)
	}

	if err := d.Check("other", version(t, "1.0.0")); !errors.Is(err, ErrNoRange) {
		t.Fatalf("no range: %v", err)
	}

	for _, tc := range []struct {
		capability, v string
		want          bool
	}{
		{"items", "1.0.0", true},
		{"streams", "1.2.0", false},
		{"streams", "1.3.0", true},
		{"marks", "1.3.0", false},
	} {
		if got := d.Has(tc.capability, "host", version(t, tc.v)); got != tc.want {
			t.Errorf("%s at %s: %v", tc.capability, tc.v, got)
		}
	}

	accepts, err := semver.ParseRange(">=0.4 !=0.4.0")
	if err != nil {
		t.Fatal(err)
	}

	if err := d.Accepts(accepts); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("the consumer leaves the version out: %v", err)
	}
}

func TestEncodingRefusesCollisions(t *testing.T) {
	d := Of[map[string]string]{Name: "x", Payload: map[string]string{"kind": "other"}}

	if _, err := json.Marshal(d); !errors.Is(err, ErrKey) {
		t.Error("a payload key the envelope writes")
	}

	d = Of[map[string]string]{Name: "x", Targets: map[string]string{"t": "*"}, Payload: map[string]string{"t": "x"}}

	if _, err := json.Marshal(d); !errors.Is(err, ErrKey) {
		t.Error("a payload key a target writes")
	}

	if _, err := json.Marshal(Description{Name: "x", Targets: map[string]string{"version": "*"}}); err == nil {
		t.Error("a target called like an envelope key")
	}

	if _, err := json.Marshal(Capability{Name: "c", Ranges: map[string]string{"name": "*"}}); err == nil {
		t.Error("a capability's target called name")
	}
}
