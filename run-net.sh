#!/usr/bin/env bash
# Host harness: build the tamago/amd64 net validate ELF, start a host-side
# raw-frame peer on a UDP socket, boot the guest headless under QEMU (q35,
# TCG) with a virtio-net-pci device whose backend is a `-netdev dgram` over
# that UDP pair, and confirm the guest's TX frame ARRIVES at the host
# (TX-OBSERVED) — the host observation net needs that blk/console don't.
#
# QEMU dgram wiring (UDP):
#   - QEMU binds  local  = 127.0.0.1:GUEST_PORT  (where it receives RX frames)
#   - QEMU sends guest TX to  remote = 127.0.0.1:HOST_PORT
# So the host peer binds HOST_PORT (to receive the guest's TX frames) and
# sends responses to GUEST_PORT (the guest's RX path). Frames on the dgram
# backend are RAW Ethernet (no length prefix).
#
# Verdict: PASS requires BOTH the guest's NET-VALIDATE: PASS line AND the host
# peer's TX-OBSERVED (magic ethertype+payload matched). RX is best-effort.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/netvalidate.elf"
SERIAL="/tmp/netvalidate_serial.log"
PEERLOG="/tmp/netvalidate_peer.log"
CPU="${CPU:-max}"
HOST_PORT="${HOST_PORT:-15810}"   # host peer binds this; QEMU's remote
GUEST_PORT="${GUEST_PORT:-15811}" # QEMU binds this (local); guest RX

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/netvalidate ) || exit 1

rm -f "$SERIAL" "$PEERLOG"

echo "== host raw-frame peer =="
# Binds HOST_PORT, receives the guest's raw TX Ethernet frames, checks our
# magic ethertype (0x88B5) + payload, logs TX-OBSERVED, then sends a raw
# response frame back to GUEST_PORT so the guest's RX path is exercised too.
python3 - "$HOST_PORT" "$GUEST_PORT" "$PEERLOG" <<'PY' &
import socket, sys, struct
host_port, guest_port, logpath = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
log = open(logpath, "w")
MAGIC_ET = 0x88B5
MAGIC_PAYLOAD = b"GO-VIRTIO-NET-VALIDATE-0123456789"
try:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", host_port))
    s.settimeout(50.0)
    log.write("peer bound 127.0.0.1:%d, awaiting guest TX frame\n" % host_port); log.flush()
    while True:
        frame, addr = s.recvfrom(65536)
        if len(frame) < 14:
            log.write("peer: runt %d-byte datagram, ignoring\n" % len(frame)); log.flush()
            continue
        et = struct.unpack(">H", frame[12:14])[0]
        body = frame[14:14+len(MAGIC_PAYLOAD)]
        log.write("peer: rx %d-byte frame from %s ethertype=%#06x body=%r\n"
                  % (len(frame), addr, et, body)); log.flush()
        if et == MAGIC_ET and body == MAGIC_PAYLOAD:
            log.write("TX-OBSERVED magic ethertype+payload matched (%d bytes)\n" % len(frame)); log.flush()
            # Send a raw response frame back to the guest (RX direction).
            # dst = the guest's source MAC (frame[6:12]); src = our dst.
            dst = frame[6:12]
            src = frame[0:6]
            resp = bytearray(60)
            resp[0:6] = dst
            resp[6:12] = src
            resp[12:14] = struct.pack(">H", MAGIC_ET)
            resp[14:14+len(MAGIC_PAYLOAD)] = MAGIC_PAYLOAD
            s.sendto(bytes(resp), ("127.0.0.1", guest_port))
            log.write("peer: sent %d-byte response frame to 127.0.0.1:%d\n" % (len(resp), guest_port)); log.flush()
            break
        else:
            log.write("peer: frame did not match magic, continuing\n"); log.flush()
except Exception as e:
    log.write("peer error: %s\n" % e); log.flush()
PY
PEERPID=$!

# Let the peer bind before QEMU starts sending.
sleep 0.5

echo "== boot =="
# disable-legacy=on forces a NON-transitional (modern-only) device so its PCI
# device ID is the modern 0x1041 (1af4:1041) go-virtio/net requires (default
# is transitional → legacy 0x1000, which pci.Probe for 0x1041 won't bind).
# The dgram backend carries RAW Ethernet frames between guest and host peer.
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -device virtio-net-pci,netdev=n0,disable-legacy=on,disable-modern=off \
  -netdev "dgram,id=n0,local.type=inet,local.host=127.0.0.1,local.port=$GUEST_PORT,remote.type=inet,remote.host=127.0.0.1,remote.port=$HOST_PORT" \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

# Wait (max ~60s) for the guest to report a PASS/FAIL verdict on serial.
for _ in $(seq 1 240); do
  if grep -q "NET-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "NET-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null
kill "$PEERPID" 2>/dev/null
wait "$PEERPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null
echo "== host peer log =="
cat "$PEERLOG" 2>/dev/null

echo "== verdict =="
GUEST_PASS=0; TX_OBSERVED=0
grep -q "NET-VALIDATE: PASS" "$SERIAL" 2>/dev/null && GUEST_PASS=1
grep -q "TX-OBSERVED" "$PEERLOG" 2>/dev/null && TX_OBSERVED=1

if [ "$GUEST_PASS" = 1 ] && [ "$TX_OBSERVED" = 1 ]; then
  echo "RESULT: PASS (guest TX frame OBSERVED on the host via a real virtio-net-pci device)"
  exit 0
fi
if grep -q "NET-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see NET-VALIDATE: FAIL line above)"
  exit 3
fi
if [ "$GUEST_PASS" = 1 ] && [ "$TX_OBSERVED" = 0 ]; then
  echo "RESULT: WALL (guest handed the frame to the device but the host peer never observed it — see peer log)"
  exit 4
fi
echo "RESULT: FAIL (no guest verdict on serial)"
exit 2
