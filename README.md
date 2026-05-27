# Meshtastic node for Waveshare USB-to-LoRa

This node is implemented on top of [Waveshare USB-to-LoRa with custom firmware](https://github.com/Archie3d/waveshare-usb-lora-firmware). It talks to the device over a serial interface. Basic Meshtastic logic is implemented.

The node connects to a [NATS server](https://github.com/nats-io/nats-server) to receive and send application specific messages. All NATS messages are serialized to JSON.

## Running with Docker

`docker-compose.yml` brings up the full stack on a Linux host: NATS broker,
`ws-node` with the USB-LoRa dongle passed in, and `ws-ntfy` forwarding
incoming text messages to ntfy.sh. The bundled `Dockerfile` is a
multi-stage build that installs `protoc`, generates the Meshtastic Go
bindings, and compiles both binaries into an Alpine runtime image.

> USB serial passthrough only works on Linux hosts. Docker Desktop on
> macOS/Windows does not pass through USB-serial devices into containers.

Prepare configuration:

```bash
cp config/node.yaml.example   config/node.yaml
cp config/ntfy.yaml.example   config/ntfy.yaml
# edit both files — at minimum set node id, names, public_key,
# nats_subject_prefix, channel keys, ntfy topic
```

Keep `nats_url: "nats://nats:4222"` in both — that's the service name on
the Compose network.

Start everything (defaults to `/dev/ttyACM0`):

```bash
docker compose up -d --build
```

If the dongle is on a different port, or if you have several USB-serial
devices, override the path. The `by-id` symlink is stable across reboots:

```bash
ls -l /dev/serial/by-id/
WS_NODE_SERIAL=/dev/serial/by-id/usb-WCH.CN_USB_Single_Serial_XXXXXXXXXX-if00 \
  docker compose up -d --build
```

Logs:

```bash
docker compose logs -f ws-node
docker compose logs -f ws-ntfy
```

### Sending / receiving from the host

The `nats` CLI is the easiest way, but you can also use the bundled
`natsio/nats-box` image without installing anything:

```bash
# Subscribe to all events from the node
docker run --rm --network=waveshare-usb-lora-node_default natsio/nats-box \
  nats sub --server=nats://nats:4222 'mesh.<your_prefix>.>'

# Publish a message (channel id and recipient as in your config)
docker run --rm --network=waveshare-usb-lora-node_default natsio/nats-box \
  nats pub --server=nats://nats:4222 mesh.<your_prefix>.out.text \
  '{"channel":1,"to":"ffffffff","text":"hello"}'
```

### Diagnosing a silent device

If `ws-node` exits with `failed to set radio standby mode: timeout`, the
custom firmware on the Waveshare board is not responding. Two helper scripts
are included:

```bash
# Listen passively, toggle DTR/RTS, then send GET_VERSION
python3 probe.py /dev/ttyACM0

# Confirm the bootloader is still listening (sends a valid chunk frame)
python3 probe_boot.py /dev/ttyACM0
```

A healthy custom firmware replies to `probe.py` with a frame starting `aa 81 ...`
(MSG_VERSION). After flashing via `wsprog`, unplug and replug **without** holding
BOOT — otherwise the bootloader stays in flash mode and the firmware never starts.

## Generating protobufs

Install Protocol Buffers compiler [as described here](https://protobuf.dev/installation/).

```shell
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
```
Make sure `$HOME/go/bin` is in the `$PATH`.

## Compiling
Using [Taskfile](https://taskfile.dev/)
```
task build
```

## Using serial port
On Linux the serial device may appear like `/dev/ttyACM0`.
To allow a non-root access:
```bash
sudo usermod -aG dialout $USER
```

## Node configuration
Node configuration should be provided as YAML file. Here is an example configuration:

```yaml
id: "12345678"      # Node unique ID (32-bits hexadecimal number)
short_name: "Nd"    # Node short name (keep it to 2-4 characters)
long_name: "My Node Name"   # Node's long name
mac_address: "AB:CD:12:34:56:78"    # Node MAC address (derived from node ID)
                                    # Deprecated, but is used for interoperability
                                    # with other Meshtastic devices
hw_model: 255       # Hardware model number, 255 corresponds to a private hardware.

public_key: "NCTMT7FWcJdyuAeJMaNMzImjRDv6nDovf/W5qIaGe/w="  # AES256 key as base64

nats_url: "nats://localhost:4222"   # URL to the NATS server
nats_subject_prefix: "mesh.my_node" # NATS messages perfix (will used at the start of all
                                    # subject names)

radio:
  frequency: 869525000      # Frequency in Hz. This value here is for the public Meshtastic
  power: 17                 # Transmission power in dBm. Allowed values: 14, 17, 20, 22
  spreading_factor: 11      # LoRa parameters
  bandwidth: 250            # Keep these values for Meshtastic LongFast communication
  coding_rate: "4/5"
  continuous_rssi: false    # Set to true to receive continuous RSSI when in RX mode

retransmit:
  forward: true             # Whether to forward received packets (public or unknown)
  period: [ "3s", "7s"]     # Outgoing packets retransmission periods
  jitter: "1s"              # Random delay (from to this value) will added to the retransmission period

channels:
  - id: 0                   # Channel ID
    name: "LongFast"        # Channel name, keep "LongFast" for Meshtastic
    encryption_key: "AQ=="  # Encryption key,this one is for public Meshtastic channel 0
  - id: 1
    name: "Private"
    encryption_key: "NRHtkaJFJyV1ftZ6GluFNR1rBr3MeqHvBmyIKaho4VY="

node_info:              # Parameters used by the Node Info app
  channel: 0            # Transmission channel number (usually 0)
  publish_period: "3h"  # Broadcast period (how often this node info will be sent out)
```

## Sending a text message
To send a message publish `{"channel":0, "to":"ffffffff", "text":"message"}` JSON to `<nats_subject_prefix>.app.text.outgoing` subject:
```bash
nats pub mesh.my_node.out.text "{\"channel\":0, \"to\":\"ffffffff\", \"text\":\"Hello\"}"
```

## Receiving text messages
To reveive messages, subscribe to `<nats_subject_prefix>.app.text.incoming`:
```bash
nats sub mesh.my_node.in.text
```

## Receiving nodes info
Discovered nodes info is publishedon `<nats_subject_prefix>.app.node_info.incoming`:
```bash
nats sub mesh.my_node.in.node_info
```

## Receiving continuous RSSI
When `continuous_rssi: true` is set in the configuration, RF signalstrength will be continuously published to `<nats_subject_prefix>.rssi` subject. Each message is a JSON object containing the timestamp (Unix time in ms) and RSSI in dBm:
```json
{"timestamp":1760828303844, "rssi": -93}
```
RSSI only gets published when device is in RX mode, during transmissions RSSI is not available.
