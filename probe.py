#!/usr/bin/env python3
"""Probe Waveshare USB-LoRa device with three tests:
1. Open and listen passively for spontaneous bytes (boot banner / continuous data).
2. Toggle DTR/RTS to reset device, then listen.
3. Send GET_VERSION request and listen.
"""
import sys
import time
import serial

PORT = sys.argv[1] if len(sys.argv) > 1 else "/dev/cu.wchusbserial5B610967921"
BAUD = 115200

START = 0xAA
ESCAPE = 0x7D
ESCAPE_START = 0x8A
ESCAPE_ESCAPE = 0x5D


def crc16(data: bytes, crc: int = 0) -> int:
    for b in data:
        a = ((crc >> 8) ^ b) & 0xFFFF
        crc = ((a << 2) ^ (a << 1) ^ a ^ (crc << 8)) & 0xFFFF
    return crc


def escape(data: bytes) -> bytes:
    out = bytearray()
    for b in data:
        if b == START:
            out += bytes([ESCAPE, ESCAPE_START])
        elif b == ESCAPE:
            out += bytes([ESCAPE, ESCAPE_ESCAPE])
        else:
            out.append(b)
    return bytes(out)


def build_frame(msg_type: int, payload: bytes = b"") -> bytes:
    body = bytes([msg_type, len(payload) & 0xFF, (len(payload) >> 8) & 0xFF]) + payload
    crc = crc16(body)
    body += bytes([crc & 0xFF, (crc >> 8) & 0xFF])
    return bytes([START]) + escape(body)


def drain(s: serial.Serial, duration: float, label: str) -> bytes:
    print(f"[{label}] listening for {duration:.1f}s...")
    deadline = time.time() + duration
    buf = bytearray()
    while time.time() < deadline:
        chunk = s.read(128)
        if chunk:
            buf += chunk
            print(f"  RX +{len(chunk)}: {chunk.hex(' ')}")
    if not buf:
        print(f"  [{label}] SILENT")
    else:
        print(f"  [{label}] total {len(buf)} bytes")
    return bytes(buf)


def main():
    print(f"opening {PORT} @ {BAUD}")
    s = serial.Serial(PORT, BAUD, timeout=0.2)
    try:
        # Test 1: passive listen for boot banner / spontaneous traffic
        drain(s, 2.0, "passive")

        # Test 2: pulse DTR/RTS low->high to simulate reset (CH343 has these wired
        # in some board variants).  Even if not wired to reset on this board, this is harmless.
        print("[reset] toggling DTR/RTS")
        s.dtr = False
        s.rts = False
        time.sleep(0.2)
        s.dtr = True
        s.rts = True
        time.sleep(0.1)
        s.reset_input_buffer()
        drain(s, 2.0, "post-reset")

        # Test 3: send GET_VERSION
        frame = build_frame(0x01)
        print(f"[probe] TX {len(frame)} bytes: {frame.hex(' ')}")
        s.reset_input_buffer()
        s.write(frame)
        s.flush()
        drain(s, 3.0, "post-tx")

    finally:
        s.close()


if __name__ == "__main__":
    main()
