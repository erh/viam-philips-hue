package hue

import (
	"context"
	"testing"

	"github.com/amimof/huego"
)

// Hue Bri has 254 levels for 255 channel values, so exactly one value must
// collide. Everything except 254 must survive the RGB -> Bri -> RGB round trip,
// and 254 reads back as full (255).
func TestBriRoundTrip(t *testing.T) {
	for m := 1; m <= 255; m++ {
		bri := maxToBri(uint8(m))
		if bri < 1 || bri > 254 {
			t.Fatalf("maxToBri(%d) = %d, out of Hue range 1-254", m, bri)
		}
		want := m
		if m == 254 {
			want = 255
		}
		if got := briToMax(bri); int(got) != want {
			t.Fatalf("briToMax(maxToBri(%d)) = %d, want %d", m, got, want)
		}
	}
}

// Setting a pure channel and reading it back through xy must return the same
// value on that channel and zero elsewhere.
func TestColorChannelRoundTrip(t *testing.T) {
	for _, m := range []uint8{1, 50, 127, 128, 200, 253, 255} {
		cases := []struct {
			name    string
			r, g, b uint8
		}{
			{"red", m, 0, 0},
			{"green", 0, m, 0},
			{"blue", 0, 0, m},
		}
		for _, c := range cases {
			x, y := rgbToXY(c.r, c.g, c.b)
			bri := maxToBri(maxUint8(c.r, c.g, c.b))
			r, g, b := xyBriToRGB([]float32{x, y}, bri)
			if maxUint8(r, g, b) != m {
				t.Errorf("%s=%d: max channel read back as %d (rgb %d,%d,%d)", c.name, m, maxUint8(r, g, b), r, g, b)
			}
			var want, got uint8
			switch c.name {
			case "red":
				want, got = c.r, r
			case "green":
				want, got = c.g, g
			case "blue":
				want, got = c.b, b
			}
			if got != want {
				t.Errorf("%s=%d: read back %d (rgb %d,%d,%d)", c.name, m, got, r, g, b)
			}
		}
	}
}

func TestNameAllocator(t *testing.T) {
	a := newNameAllocator()
	if got := a.allocate("Bedroom", "room", 81); got != "Bedroom" {
		t.Fatalf("got %q", got)
	}
	if got := a.allocate("Bedroom", "light", 1); got != "Bedroom-light" {
		t.Fatalf("room/light collision: got %q", got)
	}
	if got := a.allocate("Bedroom", "light", 2); got != "Bedroom-light-2" {
		t.Fatalf("second collision: got %q", got)
	}
	if got := a.allocate("Lamp-light", "light", 3); got != "Lamp-light" {
		t.Fatalf("got %q", got)
	}
	if got := a.allocate("Lamp-light", "light", 4); got != "Lamp-light-4" {
		t.Fatalf("name already ending in kind: got %q", got)
	}
	if got := a.allocate("", "light", 7); got != "light-7" {
		t.Fatalf("empty name: got %q", got)
	}
	if got := a.allocate("hue-mode", "mode", 0); got != "hue-mode" {
		t.Fatalf("got %q", got)
	}
}

func TestParseHexColor(t *testing.T) {
	good := map[string][3]uint8{
		"#FF8800":   {255, 136, 0},
		"ff8800":    {255, 136, 0},
		"#F80":      {255, 136, 0},
		" #000000 ": {0, 0, 0},
	}
	for in, want := range good {
		r, g, b, err := parseHexColor(in)
		if err != nil || [3]uint8{r, g, b} != want {
			t.Errorf("parseHexColor(%q) = %d,%d,%d,%v want %v", in, r, g, b, err, want)
		}
	}
	for _, bad := range []string{"", "#12345", "red", "#GGGGGG"} {
		if _, _, _, err := parseHexColor(bad); err == nil {
			t.Errorf("parseHexColor(%q) should fail", bad)
		}
	}
}

func TestPercentBri(t *testing.T) {
	for pct := 1; pct <= 100; pct++ {
		bri := percentToBri(pct)
		if bri < 1 || bri > 254 {
			t.Fatalf("percentToBri(%d) = %d", pct, bri)
		}
		if got := briToPercent(bri); got != pct {
			t.Fatalf("briToPercent(percentToBri(%d)) = %d", pct, got)
		}
	}
}

func TestOnStateFromConfig(t *testing.T) {
	if s, err := onStateFromConfig("", 0); err != nil || s != nil {
		t.Fatalf("empty config should give nil state, got %v %v", s, err)
	}
	s, err := onStateFromConfig("#FF8800", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !s.On || len(s.Xy) != 2 || s.Bri != percentToBri(50) {
		t.Fatalf("unexpected state %+v", *s)
	}
	s, err = onStateFromConfig("#800000", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Bri != maxToBri(128) {
		t.Fatalf("color-only brightness should follow max channel, got %d", s.Bri)
	}
	if _, err := onStateFromConfig("", 101); err == nil {
		t.Fatal("brightness 101 should fail")
	}
	if _, err := onStateFromConfig("nope", 0); err == nil {
		t.Fatal("bad color should fail")
	}
}

func TestPresetsHaveUniqueNames(t *testing.T) {
	seen := map[string]bool{}
	for i, p := range presets {
		if seen[p.name] {
			t.Errorf("duplicate preset %q", p.name)
		}
		seen[p.name] = true
		if i > 0 && !p.state.On {
			t.Errorf("preset %q at position %d should be an on state", p.name, i)
		}
	}
	if presets[0].name != "off" || presets[1].name != "on" {
		t.Errorf("positions 0 and 1 must be off/on")
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"Living Room":   "Living-Room",
		"  Kitchen!! ":  "Kitchen",
		"Lamp_1":        "Lamp_1",
		"Étage / Salon": "tage-Salon",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLightSupportsColor(t *testing.T) {
	cases := []struct {
		typ, mode string
		want      bool
	}{
		{"Extended color light", "ct", true},
		{"Color light", "", true},
		{"Color temperature light", "ct", false},
		{"Dimmable light", "", false},
		{"On/Off light", "", false},
		{"", "xy", true},
		{"", "hs", true},
	}
	for _, c := range cases {
		l := huego.Light{Type: c.typ}
		if c.mode != "" {
			l.State = &huego.State{ColorMode: c.mode}
		}
		if got := lightSupportsColor(l); got != c.want {
			t.Errorf("type %q mode %q: got %v, want %v", c.typ, c.mode, got, c.want)
		}
	}
}

// A configured hex color must read back (at full brightness) close to itself
// regardless of the brightness it is shown at.
func TestColorReadbackIndependentOfBrightness(t *testing.T) {
	x, y := rgbToXY(255, 136, 0)
	r, g, b := xyBriToRGB([]float32{x, y}, 254)
	if r != 255 || g < 130 || g > 142 || b > 8 {
		t.Fatalf("#FF8800 read back as %s", hexColor(r, g, b))
	}
}

func TestConfiguredStateGivesTwoPositions(t *testing.T) {
	ctx := context.Background()
	plain := &presetSwitch{}
	n, names, _ := plain.GetNumberOfPositions(ctx, nil)
	if int(n) != len(presets) || len(names) != len(presets) {
		t.Fatalf("plain switch: %d positions, %d names", n, len(names))
	}

	on, _ := onStateFromConfig("#FF8800", 60)
	fixed := &presetSwitch{onState: on}
	n, names, _ = fixed.GetNumberOfPositions(ctx, nil)
	if n != 2 || len(names) != 2 || names[0] != "off" || names[1] != "on" {
		t.Fatalf("configured switch: %d positions %v", n, names)
	}
	if err := fixed.SetPosition(ctx, 2, nil); err == nil {
		t.Fatal("position 2 should be rejected on a configured switch")
	}
}
