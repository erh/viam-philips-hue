package hue

import (
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
	if got := a.allocate("Lamp-1", 1); got != "Lamp-1" {
		t.Fatalf("got %q", got)
	}
	if got := a.allocate("Lamp-1", 2); got != "Lamp-1-2" {
		t.Fatalf("duplicate name: got %q", got)
	}
	if got := a.allocate("", 7); got != "hue-light-7" {
		t.Fatalf("empty name: got %q", got)
	}
	if got := a.allocate("hue-mode", 0); got != "hue-mode" {
		t.Fatalf("got %q", got)
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
