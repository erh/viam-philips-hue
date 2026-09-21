package hue

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/amimof/huego"
)

// parseHexColor accepts "#RRGGBB", "RRGGBB", "#RGB" or "RGB".
func parseHexColor(s string) (r, g, b uint8, err error) {
	h := strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		return 0, 0, 0, fmt.Errorf("color %q must be a hex color like #FF8800", s)
	}
	var out [3]uint8
	for i := range out {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("color %q must be a hex color like #FF8800", s)
		}
		out[i] = uint8(v)
	}
	return out[0], out[1], out[2], nil
}

func hexColor(r, g, b uint8) string {
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

// percentToBri maps 1-100 to Hue Bri 1-254.
func percentToBri(pct int) uint8 {
	if pct < 1 {
		pct = 1
	}
	if pct > 100 {
		pct = 100
	}
	bri := uint8(math.Round(float64(pct) * 254.0 / 100.0))
	if bri == 0 {
		bri = 1
	}
	return bri
}

// briToPercent maps Hue Bri 1-254 to 1-100.
func briToPercent(bri uint8) int {
	pct := int(math.Round(float64(bri) * 100.0 / 254.0))
	if pct < 1 {
		pct = 1
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// colorState builds an "on" state showing the given sRGB color at full brightness.
func colorState(r, g, b uint8) huego.State {
	x, y := rgbToXY(r, g, b)
	return huego.State{On: true, Xy: []float32{x, y}, Bri: 254}
}

// Hue Bri is 1–254 while an RGB channel is 0–255, so one value must collide.
// maxToBri clamps 255 down to 254 and briToMax reads 254 back as 255, so the
// full-brightness value (255) and every value 1–253 round-trip exactly; a
// max channel of 254 reads back as 255.
func maxToBri(maxChan uint8) uint8 {
	if maxChan > 254 {
		return 254
	}
	if maxChan == 0 {
		return 1
	}
	return maxChan
}

func briToMax(bri uint8) uint8 {
	if bri >= 254 {
		return 255
	}
	return bri
}

// rgbToXY converts sRGB values (0–255) to CIE xy chromaticity coordinates
// using the Philips Hue wide-gamut (D65) color matrix.
func rgbToXY(r, g, b uint8) (x, y float32) {
	rLin := srgbToLinear(float64(r) / 255.0)
	gLin := srgbToLinear(float64(g) / 255.0)
	bLin := srgbToLinear(float64(b) / 255.0)

	// Wide gamut D65 matrix.
	X := rLin*0.664511 + gLin*0.154324 + bLin*0.162028
	Y := rLin*0.283881 + gLin*0.668433 + bLin*0.047685
	Z := rLin*0.000088 + gLin*0.072310 + bLin*0.986039

	sum := X + Y + Z
	if sum == 0 {
		return 0, 0
	}
	return float32(X / sum), float32(Y / sum)
}

// xyBriToRGB converts CIE xy chromaticity + brightness to sRGB (0–255).
//
// Bri encodes max(r, g, b) (see maxToBri). The color direction is computed at
// Y=1 (full luminance), the brightest linear channel is normalized to 1.0,
// gamma is applied to get 8-bit sRGB at full brightness, and then every
// channel is scaled so that the brightest channel equals the decoded max.
func xyBriToRGB(xy []float32, bri uint8) (r, g, b uint8) {
	if len(xy) < 2 {
		return 0, 0, 0
	}

	x := float64(xy[0])
	y := float64(xy[1])
	if y == 0 {
		return 0, 0, 0
	}

	// Use Y=1 to extract the pure color direction regardless of stored luminance.
	X := x / y
	Z := (1 - x - y) / y

	// Wide gamut D65 inverse matrix.
	rLin := X*1.656492 - 0.354851 - Z*0.255038
	gLin := -X*0.707196 + 1.655397 + Z*0.036152
	bLin := X*0.051713 - 0.121364 + Z*1.011530

	rLin = math.Max(0, rLin)
	gLin = math.Max(0, gLin)
	bLin = math.Max(0, bLin)

	// Normalize so the brightest linear channel = 1.0, preserving hue.
	scale := math.Max(rLin, math.Max(gLin, bLin))
	if scale > 0 {
		rLin /= scale
		gLin /= scale
		bLin /= scale
	}

	// Convert to 8-bit sRGB at full brightness (max channel = 255).
	rFull := float64(linearToSRGB8(rLin))
	gFull := float64(linearToSRGB8(gLin))
	bFull := float64(linearToSRGB8(bLin))

	briF := float64(briToMax(bri)) / 255.0
	return uint8(math.Round(rFull * briF)),
		uint8(math.Round(gFull * briF)),
		uint8(math.Round(bFull * briF))
}

func maxUint8(a, b, c uint8) uint8 {
	if a >= b && a >= c {
		return a
	}
	if b >= c {
		return b
	}
	return c
}

func srgbToLinear(c float64) float64 {
	if c > 0.04045 {
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return c / 12.92
}

func linearToSRGB8(c float64) uint8 {
	var out float64
	if c > 0.0031308 {
		out = 1.055*math.Pow(c, 1/2.4) - 0.055
	} else {
		out = 12.92 * c
	}
	return uint8(math.Round(math.Min(1, math.Max(0, out)) * 255))
}
