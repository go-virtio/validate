#!/usr/bin/env bash
# Host harness: build the tamago/amd64 console validate ELF, start a host-side
# echo peer on a unix socket, boot the guest headless under QEMU (q35, TCG)
# with a virtio-serial-pci + virtconsole device whose chardev is that socket,
# and wait for the guest's TX->echo->RX round-trip verdict on the SERIAL
# console (COM1, a separate device from the virtio-console under test).
#
# The echo peer reads whatever the guest WRITES to the virtio-console and
# writes the exact same bytes back, so a token the guest writes comes back on
# its read path. The guest asserts byte-equality itself and prints
# CONSOLE-VALIDATE: PASS / FAIL. We exit non-zero on FAIL (or no verdict).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/consolevalidate.elf"
SERIAL="/tmp/consolevalidate_serial.log"
SOCK="/tmp/consolevalidate_chardev.sock"
ECHOLOG="/tmp/consolevalidate_echo.log"
CPU="${CPU:-max}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/consolevalidate ) || exit 1

rm -f "$SERIAL" "$SOCK" "$ECHOLOG"

echo "== host echo peer =="
# QEMU connects to this unix socket (chardev socket,...,server=off). The peer
# listens, accepts QEMU's connection, then echoes every byte straight back —
# so a token the guest writes to the virtio-console returns on its rx path.
python3 - "$SOCK" "$ECHOLOG" <<'PY' &
import socket, sys, os
sock_path, logpath = sys.argv[1], sys.argv[2]
log = open(logpath, "w")
try:
    if os.path.exists(sock_path):
        os.unlink(sock_path)
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(sock_path)
    srv.listen(1)
    log.write("echo peer listening on %s\n" % sock_path); log.flush()
    conn, _ = srv.accept()
    log.write("echo peer: QEMU connected\n"); log.flush()
    total = 0
    while True:
        data = conn.recv(65536)
        if not data:
            break
        conn.sendall(data)
        total += len(data)
        log.write("echo peer: echoed %d bytes (total %d): %r\n"
                  % (len(data), total, data)); log.flush()
except Exception as e:
    log.write("echo peer error: %s\n" % e); log.flush()
PY
ECHOPID=$!

# Give the echo peer a moment to create + bind the listening socket before
# QEMU tries to connect to it.
for _ in $(seq 1 40); do
  [ -S "$SOCK" ] && break
  sleep 0.05
done
if [ ! -S "$SOCK" ]; then
  echo "RESULT: FAIL (host echo peer never created its socket)"
  kill "$ECHOPID" 2>/dev/null
  exit 2
fi

echo "== boot =="
# virtio-serial-pci is the virtio-console transport device; virtconsole is the
# port bound to our echo chardev. disable-legacy=on forces a NON-transitional
# (modern-only) device so its PCI device ID is the modern 0x1043 (1af4:1043)
# that go-virtio/console requires (the default is transitional → legacy 0x1003,
# which pci.Probe for 0x1043 will not bind). server=off makes QEMU the client
# that connects to the host echo peer's listening socket above.
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -device virtio-serial-pci,disable-legacy=on,disable-modern=off,id=vser \
  -device virtconsole,chardev=c0,name=org.go-virtio.console.0 \
  -chardev "socket,id=c0,path=$SOCK,server=off" \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

# Wait (max ~60s) for the guest to report a PASS/FAIL verdict on serial.
for _ in $(seq 1 240); do
  if grep -q "CONSOLE-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "CONSOLE-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null
kill "$ECHOPID" 2>/dev/null
wait "$ECHOPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null
echo "== echo peer log =="
cat "$ECHOLOG" 2>/dev/null

echo "== verdict =="
if grep -q "CONSOLE-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then
  echo "RESULT: PASS (console TX->host-echo->RX round-trip byte-equal on a real virtio-console-pci device)"
  exit 0
fi
if grep -q "CONSOLE-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see CONSOLE-VALIDATE: FAIL line above)"
  exit 3
fi
echo "RESULT: FAIL (no verdict on serial — guest never reported)"
exit 2
