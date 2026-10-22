package hue

import (
	"encoding/binary"
	"math"
)

// HueStream v1 framing, as used by the Hue Entertainment API over DTLS:
//
//	"HueStream"            9 bytes, ASCII protocol name
//	0x01 0x00              API version 1.0
//	sequence               1 byte (ignored by the bridge)
//	0x00 0x00              reserved
//	color space            1 byte, 0x00 = RGB, 0x01 = XY + brightness
//	0x00                   reserved
//
// followed by one 9-byte block per light:
//
//	0x00                   device type (0x00 = light)
//	light id               uint16 big-endian (the v1 light id)
//	R G B                  3 x uint16 big-endian, 0-65535
const (
	hueStreamProtocol    = "HueStream"
	hueStreamHeaderLen   = 16
	hueStreamLightLen    = 9
	hueStreamColorRGB    = 0x00
	hueStreamDeviceLight = 0x00
)

// streamLight is one light's encoded color in a HueStream frame.
type streamLight struct {
	id      uint16
	r, g, b uint16
}

// encodeHueStreamV1 builds a HueStream v1 RGB message for the given lights.
func encodeHueStreamV1(seq byte, lights []streamLight) []byte {
	buf := make([]byte, hueStreamHeaderLen+hueStreamLightLen*len(lights))
	copy(buf, hueStreamProtocol)
	buf[9] = 0x01  // major version
	buf[10] = 0x00 // minor version
	buf[11] = seq
	// buf[12], buf[13] reserved
	buf[14] = hueStreamColorRGB
	// buf[15] reserved
	off := hueStreamHeaderLen
	for _, l := range lights {
		buf[off] = hueStreamDeviceLight
		binary.BigEndian.PutUint16(buf[off+1:], l.id)
		binary.BigEndian.PutUint16(buf[off+3:], l.r)
		binary.BigEndian.PutUint16(buf[off+5:], l.g)
		binary.BigEndian.PutUint16(buf[off+7:], l.b)
		off += hueStreamLightLen
	}
	return buf
}

// srgbToLinear16 converts one sRGB channel value (0-255) to linear light with
// the sRGB transfer function, scales it by brightness (0-1), and returns it as
// the 16-bit value HueStream expects.
func srgbToLinear16(v, brightness float64) uint16 {
	c := clamp01(v / 255)
	var lin float64
	if c <= 0.04045 {
		lin = c / 12.92
	} else {
		lin = math.Pow((c+0.055)/1.055, 2.4)
	}
	return uint16(math.Round(clamp01(lin*clamp01(brightness)) * 65535))
}

func clamp01(v float64) float64 {
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
