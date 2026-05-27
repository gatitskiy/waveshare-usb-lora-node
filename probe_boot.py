#!/usr/bin/env python3
"""Probe whether the custom bootloader is alive.

The bootloader protocol (from wsprog):
- 0x10 (X_START) + 2 CRC bytes (LE) + 1024-byte chunk
- Response: 0x30 (ACK), 0x40 (NACK), or 0x50 (NCRC) + 2 CRC bytes

We send a chunk of zeros with the CORRECT crc16. If bootloader is alive we
should get a single byte ACK (0x30) or NACK (0x40). NCRC would indicate we
miscalculated the CRC — but still proves the bootloader is talking.
"""
import sys
import time
import serial

PORT = sys.argv[1] if len(sys.argv) > 1 else "/dev/cu.wchusbserial5B610967921"
BAUD = 115200
CHUNK_SIZE = 1024


def crc16(data: bytes, crc: int = 0) -> int:
    for b in data:
        a = ((crc >> 8) ^ b) & 0xFFFF
        crc = ((a << 2) ^ (a << 1) ^ a ^ (crc << 8)) & 0xFFFF
    return crc


def main():
    print(f"opening {PORT} @ {BAUD}")
    s = serial.Serial(PORT, BAUD, timeout=3)
    try:
        s.reset_input_buffer()
        chunk = bytes(CHUNK_SIZE)  # all zeros
        crc = crc16(chunk)
        print(f"[boot] sending X_START + crc=0x{crc:04x} + 1024 zero bytes")
        s.write(bytes([0x10]) + crc.to_bytes(2, "little") + chunk)
        s.flush()

        print("[boot] waiting up to 5s for response...")
        deadline = time.time() + 5.0
        buf = bytearray()
        while time.time() < deadline:
            chunk_in = s.read(8)
            if chunk_in:
                buf += chunk_in
                print(f"  RX +{len(chunk_in)}: {chunk_in.hex(' ')}")
                if len(buf) >= 1:
                    code = buf[0]
                    name = {0x30: "ACK", 0x40: "NACK", 0x50: "NCRC"}.get(code, "UNKNOWN")
                    print(f"  -> first byte 0x{code:02x} = {name}")
                    break
        if not buf:
            print("[boot] SILENT — bootloader not responding either")
    finally:
        s.close()


if __name__ == "__main__":
    main()
