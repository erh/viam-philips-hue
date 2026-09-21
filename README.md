# Module viam-philips-hue

A Viam module for controlling Philips Hue smart lights.

## Setup

You need a Philips Hue Bridge and an API username (key). Configure them once
in a `hue-bridge` component and every other model references it by name:

```json
{
  "components": [
    {
      "name": "hue-bridge",
      "api": "rdk:component:generic",
      "model": "erh:viam-philips-hue:hue-bridge",
      "attributes": { "bridge_host": "192.168.1.100", "username": "your-api-username-here" }
    },
    {
      "name": "Bedroom",
      "api": "rdk:component:switch",
      "model": "erh:viam-philips-hue:hue-light",
      "attributes": { "bridge": "hue-bridge", "light_id": 1 }
    }
  ]
}
```

Every model also accepts `bridge_host` and `username` directly instead of
`bridge`, for a one-off component. If `bridge_host` is left out the module
looks the bridge up through Philips' cloud discovery service. That service is
rate limited and needs internet access, so setting `bridge_host` is
recommended. If `username` is missing or the bridge cannot be found, the
configuration error includes the instructions below.

### Finding your bridge IP

- Hue app: Settings > My Hue System > (your bridge) > the (i) icon shows the IP address
- Open <https://discovery.meethue.com> in a browser; it lists bridges on your network
- Look for a device named `Philips-hue` in your router's DHCP client list
- Run `./bin/huecli -register`; it prints the discovered bridge IP

### Getting a username

Use the CLI tool to register with your bridge:

```bash
# Build the CLI
go build -o bin/huecli cmd/cli/cmd.go

# Register (will auto-discover your bridge)
./bin/huecli -register
```

This will:

1. Auto-discover your Hue bridge on the network
2. Prompt you to press the link button on the bridge
3. Create and display your username

Save the username for your Viam config.

### Manual registration

1. Find your bridge IP (see above)
2. Press the link button on your Hue Bridge
3. Within 30 seconds, run: `curl -X POST http://<bridge-ip>/api -d '{"devicetype":"viam#module"}'`
4. The response will contain your username

## Quick start with discovery

1. Add a `hue-discovery` service with your `username` (and `bridge_host`).
2. Open the service's **Test** panel. It lists ready-to-add configs: one
   `hue-bridge`, one `hue-room` per room or zone in the Hue app, one
   `hue-light` per bulb, and one `hue-lights-mode` switch.
3. Add the ones you want. Rooms are usually all you need for day-to-day
   control; add individual lights only where you want per-bulb control.

For five bulbs in two rooms that is about ten one-line components instead of
one per bulb per color channel.

## hue-bridge

Generic component that owns the bridge connection. Other models reference it
with `"bridge": "<name>"`.

```json
{
  "bridge_host": "192.168.1.100",
  "username": "your-api-username-here"
}
```

`DoCommand` supports `{"command": "lights"}` and `{"command": "groups"}` to
list what the bridge knows about, which is handy for finding IDs.

## hue-light

One bulb as a switch. Positions are named presets, and `DoCommand` sets any
color, brightness, or color temperature.

```json
{
  "bridge": "hue-bridge",
  "light_id": 1
}
```

To have the light set to a color and brightness when the component starts
(and whenever position 1, `on`, is selected), add `color` (hex) and/or
`brightness` (1-100):

```json
{
  "bridge": "hue-bridge",
  "light_id": 1,
  "color": "#FF8800",
  "brightness": 60
}
```

With `color` alone the brightness follows the color's brightest channel, so
`#800000` is a dim red and `#FF0000` a full red.

A light configured this way is a plain two-position switch: `off` and `on`.
The preset positions below only apply when neither `color` nor `brightness`
is set. `DoCommand` works either way.

### Switch positions

| Position | Name        | Effect                                                    |
| -------- | ----------- | --------------------------------------------------------- |
| 0        | `off`       | Off                                                       |
| 1        | `on`        | On, at the configured `color`/`brightness` if set         |
| 2        | `dim`       | 25% brightness                                            |
| 3        | `bright`    | 100% brightness                                           |
| 4        | `warm`      | Warm white (~2700 K) at 80%                               |
| 5        | `daylight`  | Cool daylight white (~6500 K) at 100%                     |
| 6        | `white`     | Neutral white (~4000 K) at 100%                           |
| 7-14     | `red`, `orange`, `yellow`, `green`, `cyan`, `blue`, `purple`, `pink` | Full-brightness colors |
| 15       | `colorloop` | Cycle through colors                                      |

The names are returned by `GetNumberOfPositions`, so clients can show them.

### DoCommand

Any combination of these keys is applied in one request:

```json
{ "color": "#FF8800", "brightness": 60 }
{ "color": [255, 136, 0] }
{ "kelvin": 2700 }
{ "ct": 370 }
{ "effect": "colorloop" }
{ "on": false }
{ "brightness": 40, "transition": 2 }
```

`brightness` is 0-100 (0 turns the light off), `kelvin` is 2000-6500, `ct`
is mireds (153-500), and `transition` is the fade time in seconds. The
response, and `GetStatus`, report the current state. `color` is the hue at
full brightness, independent of `brightness`; `ct`/`kelvin` appear instead
of `color` when the light is in color temperature mode.

```json
{ "on": true, "brightness": 60, "color": "#FF8800", "colormode": "xy", "effect": "none", "position": 1, "preset": "on" }
```

## hue-room

A Hue room or zone controlled as one switch. Same positions and `DoCommand`
as `hue-light`, but one bridge call changes every bulb in the group together.

```json
{
  "bridge": "hue-bridge",
  "group_id": 3
}
```

Or by the name shown in the Hue app:

```json
{
  "bridge": "hue-bridge",
  "group_name": "Living room"
}
```

Group `0` is the bridge's built-in "all lights" group, so
`{"bridge": "hue-bridge"}` alone controls every light. `color` and
`brightness` work the same way as for `hue-light`.

## hue-lights-mode

Controls pre-defined lighting modes across one or more lights. The default mode is `"none"`, which restores lights to their state before any mode was activated. When switching to a mode, the current light state is automatically saved so it can be restored when returning to `"none"`.

```json
{
  "bridge": "hue-bridge",
  "dance": {
    "left": [1, 2],
    "center": [3, 4],
    "right": [5]
  },
  "daylight": [1, 2, 3],
  "warm": [1, 2, 3]
}
```

Each mode key takes an array of Hue light IDs to control when that mode is active. Only the modes you want to use need to be configured. Discovery fills `dance` with one group per room.

### Switch Positions

- Position 0 (`"none"`): Restore all lights to their saved pre-mode state
- Position 1 (`"dance"`): Staggered color-loop across light groups
- Position 2 (`"daylight"`): Cool daylight white (~6500 K) at full brightness
- Position 3 (`"warm"`): Warm incandescent white (~2700 K) at moderate brightness

Switching straight from one mode to another (for example `dance` to `daylight`) keeps the original pre-mode snapshot, so position 0 always restores the lights to how they were before the first mode was activated.

### Dance mode light groups

The `dance` config takes a **map of group name → light IDs**. All lights in a group are kept in sync with each other. Groups are sorted alphabetically by name and then evenly offset around the full hue wheel (0–65535), so different groups always display different colors.

## hue-discovery

Discovery service that lists everything on your bridge as ready-to-add configs.

```json
{
  "username": "your-api-username-here",
  "bridge_host": "192.168.1.100"
}
```

Or, if you already have a `hue-bridge` component:

```json
{
  "bridge": "hue-bridge"
}
```

It emits:

- a `hue-bridge` (only when configured with inline credentials), named `hue-bridge`
- one `hue-room` per room and zone, named after the room
- one `hue-light` per bulb, named after the bulb
- one `hue-lights-mode` named `hue-mode`, with a dance group per room and daylight/warm covering every white-capable light

Names are sanitized (spaces and punctuation become `-`) and made unique by appending the Hue ID on collisions. Color capability is taken from the light's type, so a color bulb currently showing white is still detected as a color light.

`DiscoverResources` accepts extras to trim the output: `{"lights": false}`, `{"rooms": false}`, or `{"mode": false}`.

## Deprecated models

`hue-light-brightness` (on/off and brightness as positions 0-100) and
`hue-light-color` (one switch per RGB channel) still work but are superseded
by `hue-light`, which does both in one component. They are no longer emitted
by discovery and will be removed in a future release.

## CLI Usage

```bash
# Register with the bridge (get a username)
./bin/huecli -register

# List everything discovery would emit
./bin/huecli -username YOUR_USERNAME

# Control a device by its discovered name. Names are sanitized (spaces and
# punctuation become "-"). Position numbers are listed above.
./bin/huecli -username YOUR_USERNAME -device Living-room -set 4

# Skip cloud discovery by passing the bridge IP directly
./bin/huecli -bridge 192.168.1.100 -username YOUR_USERNAME
```
