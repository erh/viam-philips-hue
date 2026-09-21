package hue

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amimof/huego"
	"go.viam.com/rdk/components/generic"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/discovery"
	"go.viam.com/rdk/utils"
)

var HueDiscovery = family.WithModel("hue-discovery")

func init() {
	resource.RegisterService(discovery.API, HueDiscovery,
		resource.Registration[discovery.Service, *DiscoveryConfig]{
			Constructor: newHueDiscover,
		},
	)
}

// DiscoveryConfig needs nothing. Without bridge_host the bridge is found on
// the network; without username the service walks you through pressing the
// link button and creates one.
type DiscoveryConfig struct {
	BridgeHost string `json:"bridge_host,omitempty"`
	Username   string `json:"username,omitempty"`
}

func (cfg *DiscoveryConfig) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

// deviceType is the name the created API key is registered under on the bridge.
const deviceType = "viam#hue-module"

func NewDiscovery(logger logging.Logger) *HueDiscover {
	return &HueDiscover{logger: logger}
}

// CreateUser creates a new user on the Hue bridge. The link button must be pressed first.
func CreateUser(bridgeHost, deviceType string) (string, error) {
	bridge := huego.New(bridgeHost, "")
	user, err := bridge.CreateUser(deviceType)
	if err != nil {
		return "", err
	}
	return user, nil
}

// SetBridge points the discovery at a bridge using known credentials (CLI use).
func (s *HueDiscover) SetBridge(host, username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = host
	s.username = username
	s.bridge = huego.New(host, username)
}

type HueDiscover struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger

	mu       sync.Mutex
	host     string        // bridge IP once known
	username string        // API key once known
	bridge   *huego.Bridge // set once host and username are both known
	status   string        // what the user should do next, if anything

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newHueDiscover(ctx context.Context, _ resource.Dependencies, rawConf resource.Config, logger logging.Logger) (discovery.Service, error) {
	conf, err := resource.NativeConfig[*DiscoveryConfig](rawConf)
	if err != nil {
		return nil, err
	}

	s := &HueDiscover{
		name:     rawConf.ResourceName(),
		logger:   logger,
		host:     conf.BridgeHost,
		username: conf.Username,
	}

	if conf.Username != "" {
		// Credentials given: connect now and fail loudly if they are wrong.
		host, err := resolveBridgeHost(conf.BridgeHost, logger)
		if err != nil {
			return nil, err
		}
		bridge := huego.New(host, conf.Username)
		if _, err := bridge.GetConfigContext(ctx); err != nil {
			return nil, fmt.Errorf("cannot connect to Hue bridge at %s: %w\n%s", host, err, SetupHelp)
		}
		s.host = host
		s.bridge = bridge
		return s, nil
	}

	// No username: find the bridge and register with it in the background so
	// the service comes up immediately and the instructions land in the logs.
	bgCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.register(bgCtx)
	}()

	return s, nil
}

func (s *HueDiscover) Close(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}

func (s *HueDiscover) setStatus(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// register finds the bridge (if bridge_host was not given), then polls the
// bridge's create-user endpoint until the link button has been pressed,
// logging what to do at each step. On success it logs the key and what to
// do with it, and DiscoverResources starts returning configs.
func (s *HueDiscover) register(ctx context.Context) {
	const (
		findRetry  = 15 * time.Second
		pollEvery  = 2 * time.Second
		remindEach = 20 * time.Second
	)

	s.mu.Lock()
	host := s.host
	s.mu.Unlock()

	for host == "" {
		s.setStatus("Looking for the Hue bridge on the network (mDNS, then Philips discovery)...")
		s.logger.Info(s.currentStatus())
		found, err := DiscoverBridge(s.logger)
		if err == nil {
			host = found
			s.mu.Lock()
			s.host = host
			s.mu.Unlock()
			s.logger.Infof("Found Hue bridge at %s", host)
			break
		}
		s.setStatus(fmt.Sprintf("No Hue bridge found on the network; retrying in %s. Set bridge_host to skip discovery.", findRetry))
		s.logger.Warnf("%v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(findRetry):
		}
	}

	instruction := fmt.Sprintf("No username configured. Press the round link button on the Hue bridge at %s; this service checks every %s and will log the API key once the button has been pressed.", host, pollEvery)
	s.setStatus(instruction)
	s.logger.Warn(instruction)

	bridge := huego.New(host, "")
	lastRemind := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}

		username, err := bridge.CreateUserContext(ctx, deviceType)
		if err == nil && username != "" {
			s.mu.Lock()
			s.username = username
			s.bridge = huego.New(host, username)
			s.status = ""
			s.mu.Unlock()

			s.logger.Infof("Registered with the Hue bridge at %s. Your API key (username) is: %s", host, username)
			s.logger.Infof("Next: open this service's Test panel and add the hue-bridge component it lists first (it already contains this key), then add the rooms and lights. Also put the key in this service's username attribute so it survives restarts.")
			return
		}

		var apiErr *huego.APIError
		if errors.As(err, &apiErr) && apiErr.Type == 101 {
			// Link button not pressed yet; keep waiting, remind occasionally.
			if time.Since(lastRemind) >= remindEach {
				s.logger.Warn(instruction)
				lastRemind = time.Now()
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.setStatus(fmt.Sprintf("Bridge at %s is not answering registration (%v); still trying. If the IP is wrong, set bridge_host.", host, err))
		s.logger.Warnf("Hue bridge registration attempt failed: %v", err)
	}
}

func (s *HueDiscover) currentStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *HueDiscover) Name() resource.Name {
	return s.name
}

func (s *HueDiscover) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return s.Status(ctx)
}

func (s *HueDiscover) Status(ctx context.Context) (map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]interface{}{
		"bridge_host": s.host,
		"registered":  s.username != "",
	}
	if s.status != "" {
		out["next_step"] = s.status
	}
	return out, nil
}

// DiscoverResources emits configs for everything on the bridge, hue-bridge
// first. Until the bridge is registered it returns an error saying what to
// do. Optional extras:
//
//	"rooms":  false  -> skip the per-room switches
//	"lights": false  -> skip the per-light switches
//	"mode":   false  -> skip the lights-mode switch
func (s *HueDiscover) DiscoverResources(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	return s.DiscoverHue(ctx, extra)
}

// sanitizeName replaces any character that is not alphanumeric, '-', or '_'
// with '-', then collapses runs of '-' and trims leading/trailing '-'.
var reUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
var reCollapse = regexp.MustCompile(`-{2,}`)

func sanitizeName(name string) string {
	s := reUnsafe.ReplaceAllString(name, "-")
	s = reCollapse.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// nameAllocator hands out unique resource names. On a collision (a bulb named
// the same as its room, or two bulbs with the same name) the kind is appended
// first ("Bedroom-light"), then the Hue ID ("Bedroom-light-3"). A name that
// sanitizes to nothing falls back to "<kind>-<id>".
type nameAllocator struct {
	used map[string]bool
}

func newNameAllocator() *nameAllocator {
	return &nameAllocator{used: map[string]bool{}}
}

func (a *nameAllocator) allocate(base, kind string, id int) string {
	if base == "" {
		base = fmt.Sprintf("%s-%d", kind, id)
	}
	// A name that already ends in the kind ("Lamp-light") gets the ID only.
	stem := base
	if strings.HasSuffix(strings.ToLower(base), "-"+kind) {
		stem = base[:len(base)-len(kind)-1]
	}
	candidates := []string{
		base,
		fmt.Sprintf("%s-%s", stem, kind),
		fmt.Sprintf("%s-%s-%d", stem, kind, id),
	}
	name := ""
	for _, c := range candidates {
		if !a.used[c] {
			name = c
			break
		}
	}
	for i := 2; name == ""; i++ {
		c := fmt.Sprintf("%s-%s-%d-%d", stem, kind, id, i)
		if !a.used[c] {
			name = c
		}
	}
	a.used[name] = true
	return name
}

func extraBool(extra map[string]any, key string, def bool) bool {
	if extra == nil {
		return def
	}
	if v, ok := extra[key].(bool); ok {
		return v
	}
	return def
}

func (s *HueDiscover) DiscoverHue(ctx context.Context, extra map[string]any) ([]resource.Config, error) {
	s.mu.Lock()
	bridge, host, username, status := s.bridge, s.host, s.username, s.status
	s.mu.Unlock()

	if bridge == nil {
		if status == "" {
			status = "Hue bridge is not registered yet; check this service's logs."
		}
		return nil, errors.New(status)
	}

	lights, err := bridge.GetLightsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get lights from Hue bridge: %w", err)
	}
	groups, err := bridge.GetGroupsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot get groups from Hue bridge: %w", err)
	}
	sort.Slice(lights, func(i, j int) bool { return lights[i].ID < lights[j].ID })
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })

	configs := []resource.Config{}
	names := newNameAllocator()

	// The bridge comes first: everything else references it by name.
	bridgeName := names.allocate("hue-bridge", "bridge", 0)
	configs = append(configs, resource.Config{
		Name:  bridgeName,
		API:   generic.API,
		Model: HueBridge,
		Attributes: utils.AttributeMap{
			"bridge_host": host,
			"username":    username,
		},
	})

	colorIDs := map[int]bool{}
	ctIDs := map[int]bool{}
	for _, light := range lights {
		if lightSupportsColor(light) {
			colorIDs[light.ID] = true
		}
		if colorIDs[light.ID] || strings.Contains(strings.ToLower(light.Type), "temperature") {
			ctIDs[light.ID] = true
		}
	}

	// Rooms and zones, one switch each.
	danceGroups := map[string][]int{}
	if extraBool(extra, "rooms", true) {
		for _, g := range groups {
			if g.Type != "Room" && g.Type != "Zone" {
				continue
			}
			name := names.allocate(sanitizeName(g.Name), "room", g.ID)
			s.logger.Debugf("discovery result group: %d %s type: %s lights: %v", g.ID, g.Name, g.Type, g.Lights)
			configs = append(configs, resource.Config{
				Name:  name,
				API:   toggleswitch.API,
				Model: HueRoom,
				Attributes: utils.AttributeMap{
					"bridge":   bridgeName,
					"group_id": g.ID,
				},
			})
			if g.Type == "Room" {
				for _, id := range groupLightIDs(g) {
					if colorIDs[id] {
						danceGroups[name] = append(danceGroups[name], id)
					}
				}
			}
		}
	}

	// Individual lights.
	if extraBool(extra, "lights", true) {
		for _, light := range lights {
			s.logger.Debugf("discovery result light: %d %s type: %s color: %v", light.ID, light.Name, light.Type, colorIDs[light.ID])
			configs = append(configs, resource.Config{
				Name:  names.allocate(sanitizeName(light.Name), "light", light.ID),
				API:   toggleswitch.API,
				Model: HueLight,
				Attributes: utils.AttributeMap{
					"bridge":   bridgeName,
					"light_id": light.ID,
				},
			})
		}
	}

	// A single mode switch: dance groups follow rooms so each room cycles at a
	// different point on the hue wheel; daylight/warm cover every light that
	// supports color temperature.
	if extraBool(extra, "mode", true) && (len(colorIDs) > 0 || len(ctIDs) > 0) {
		if len(danceGroups) == 0 && len(colorIDs) > 0 {
			danceGroups["all"] = sortedIDs(colorIDs)
		}
		attrs := utils.AttributeMap{"bridge": bridgeName}
		if len(danceGroups) > 0 {
			attrs["dance"] = danceGroups
		}
		if len(ctIDs) > 0 {
			attrs["daylight"] = sortedIDs(ctIDs)
			attrs["warm"] = sortedIDs(ctIDs)
		}
		configs = append(configs, resource.Config{
			Name:       names.allocate("hue-mode", "mode", 0),
			API:        toggleswitch.API,
			Model:      HueLightMode,
			Attributes: attrs,
		})
	}

	return configs, nil
}

func sortedIDs(set map[int]bool) []int {
	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
