# Module viam-philips-hue

A Viam module for controlling Philips Hue smart lights.

## Setup

You need a Philips Hue Bridge and an API username (key).

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

If you prefer to do it manually:

1. Find your bridge IP (check your router or use the Hue app)
2. Press the link button on your Hue Bridge
3. Within 30 seconds, run: `curl -X POST http://<bridge-ip>/api -d '{"devicetype":"viam#module"}'`
4. The response will contain your username

## hue-discovery

Discovery service that finds all lights connected to your Hue Bridge. The bridge IP will be discovered automatically if not specified.

```json
{
  "username": "your-api-username-here"
}
```

Or with explicit bridge host:
```json
{
  "bridge_host": "192.168.1.100",
  "username": "your-api-username-here"
}
```

## hue-light

Controls a Philips Hue light. Implements the switch interface with brightness control. The bridge IP will be discovered automatically if not specified.

```json
{
  "username": "your-api-username-here",
  "light_id": 1
}
```

Or with explicit bridge host:
```json
{
  "bridge_host": "192.168.1.100",
  "username": "your-api-username-here",
  "light_id": 1
}
```

### Switch Positions

- Position 0: Light off
- Position 1: Light on (current brightness)
- Position 2-100: Light on at that brightness percentage

## CLI Usage

```bash
# Register with the bridge (get a username)
./bin/huecli -register

# List all lights
./bin/huecli -username YOUR_USERNAME

# Control a specific light
./bin/huecli -username YOUR_USERNAME -device "Living Room" -set 50
```
