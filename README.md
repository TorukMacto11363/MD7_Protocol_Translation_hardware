# MD7 Bridge - Hardware Implementation

# Architecture

dtnclient -> dtnd1 -> Bridge1 -> [Python sidecar] -> Heltec fa4371a4 -> 868 MHz LoRa -> Heltec fa6c00d0 -> [Python sidecar] -> Bridge2 -> dtnd2 -> dtnclient

# Fragment encoding - why binary not JSON

JSON was the  first choice for encoding a fragment - easy to read, easy to debug. It doesn't
work here because the encoded packet size is too large for what this hardware can reliably send, 114–230 bytes per packet: way too big for what this hardware can reliably send.

The binary format packs the same info into a 13-byte header.

Refer makefile to run the hardware test.

# constraints

EU radio law limits transmission to 1% of time per channel.

