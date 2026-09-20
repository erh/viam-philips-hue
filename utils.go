package hue

import (
	"fmt"
	"strings"
	"sync"

	"github.com/amimof/huego"
	"go.viam.com/rdk/logging"
)

// SetupHelp explains how to find the Hue bridge IP and obtain an API username.
// It is appended to configuration errors so the fix is visible right where the
// error shows up.
const SetupHelp = `How to find your Hue bridge IP:
  - Hue app: Settings > My Hue System > (your bridge) > (i) icon shows the IP address
  - Open https://discovery.meethue.com in a browser; it lists bridges on your network
  - Look for a device named "Philips-hue" in your router's DHCP client list
  - Or run the CLI: go run ./cmd/cli -register (auto-discovers the bridge and creates a username)
How to get a username (API key): press the link button on the bridge, then within 30 seconds run:
  curl -X POST http://<bridge-ip>/api -d '{"devicetype":"viam#module"}'
Then set "bridge_host" and "username" in this component's attributes.`

// errMissingUsername is returned by every model's Validate when username is empty.
func errMissingUsername() error {
	return fmt.Errorf("need a username (API key) for the Hue bridge\n%s", SetupHelp)
}

// The discovery endpoint (https://discovery.meethue.com) is rate limited, and
// huego.Discover returns an empty Bridge (not an error) when it lists nothing.
// Cache the first successful result so every resource without a bridge_host
// shares one lookup per process.
var (
	discoverMu     sync.Mutex
	discoveredHost string
)

// DiscoverBridge finds a Hue bridge on the network and returns its host address.
// It returns an error (with setup instructions) when no bridge is found.
func DiscoverBridge() (string, error) {
	bridge, err := huego.Discover()
	if err != nil {
		return "", fmt.Errorf("failed to discover Hue bridge: %w\n%s", err, SetupHelp)
	}
	if bridge == nil || bridge.Host == "" {
		return "", fmt.Errorf("no Hue bridge found via discovery; set bridge_host explicitly\n%s", SetupHelp)
	}
	return bridge.Host, nil
}

// resolveBridgeHost returns bridgeHost when set, otherwise discovers (and caches) one.
func resolveBridgeHost(bridgeHost string, logger logging.Logger) (string, error) {
	if bridgeHost != "" {
		return bridgeHost, nil
	}

	discoverMu.Lock()
	defer discoverMu.Unlock()
	if discoveredHost != "" {
		return discoveredHost, nil
	}

	logger.Info("No bridge_host specified, discovering Hue bridge...")
	host, err := DiscoverBridge()
	if err != nil {
		return "", err
	}
	discoveredHost = host
	logger.Infof("Discovered Hue bridge at %s", host)
	return host, nil
}

// connectToLight resolves the bridge host (discovering it if empty), connects to
// the bridge, and verifies the target light is reachable.
func connectToLight(bridgeHost, username string, lightID int, logger logging.Logger) (*huego.Bridge, *huego.Light, error) {
	bridgeHost, err := resolveBridgeHost(bridgeHost, logger)
	if err != nil {
		return nil, nil, err
	}

	bridge := huego.New(bridgeHost, username)
	light, err := bridge.GetLight(lightID)
	if err != nil {
		return nil, nil, fmt.Errorf("can't get light %d from Hue bridge @ (%s): %w", lightID, bridgeHost, err)
	}
	if light.State == nil {
		return nil, nil, fmt.Errorf("light %d on Hue bridge @ (%s) returned no state", lightID, bridgeHost)
	}

	return bridge, light, nil
}

// lightSupportsColor reports whether a light can show colors, based on its
// declared type rather than its current color mode (a color bulb sitting in
// "ct" mode is still a color bulb).
func lightSupportsColor(light huego.Light) bool {
	t := strings.ToLower(light.Type)
	if strings.Contains(t, "color") && !strings.Contains(t, "color temperature") {
		return true
	}
	if light.State != nil {
		if light.State.ColorMode == "xy" || light.State.ColorMode == "hs" {
			return true
		}
	}
	return false
}
