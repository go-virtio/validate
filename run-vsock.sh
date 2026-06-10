#!/usr/bin/env bash
# Host harness: build the tamago/amd64 vsock validate ELF, then attempt to boot
# the guest headless under QEMU (q35, TCG) with a real vhost-vsock-pci device.
#
# HARD HOST DEPENDENCY: virtio-vsock's QEMU PCI front-end (`vhost-vsock-pci`)
# is backed by the HOST kernel's vhost_vsock module (AF_VSOCK, /dev/vhost-vsock).
# That facility is Linux-only. On macOS there is no host vsock and the Homebrew
# macOS QEMU build does not even compile `vhost-vsock-pci` in, so QEMU rejects
# the device at instantiation and the guest never boots. This script DETECTS
# that wall and reports it precisely (MAPPED-WALL) rather than faking a result.
#
# On a Linux KVM host with `modprobe vhost_vsock`, this script DOES run the
# round-trip: it binds an AF_VSOCK listener on (VMADDR_CID_HOST, PORT), boots
# the guest, the guest sends an OpRequest and the host listener replies, and the
# guest asserts the reply round-tripped back to its guest_cid, printing
# VSOCK-VALIDATE: PASS.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/vsockvalidate.elf"
SERIAL="/tmp/vsockvalidate_serial.log"
CPU="${CPU:-max}"
GUEST_CID="${GUEST_CID:-42}"
PORT="${PORT:-9999}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/vsockvalidate ) || exit 1

rm -f "$SERIAL"

echo "== host vhost-vsock availability probe =="
# 1) Is the device even compiled into this QEMU build?
if ! qemu-system-x86_64 -device help 2>/dev/null | grep -q 'vhost-vsock-pci'; then
  echo "MAPPED-WALL: this QEMU build has no 'vhost-vsock-pci' device model."
  echo "             vhost-vsock requires the Linux host kernel module vhost_vsock"
  echo "             (/dev/vhost-vsock, AF_VSOCK); the macOS QEMU build compiles it out."
  echo "             Concrete QEMU error when forced:"
  qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 256M -display none -no-reboot \
    -device "vhost-vsock-pci,guest-cid=$GUEST_CID" -S -monitor none -serial none 2>&1 \
    | sed 's/^/             /' | head -4
  echo "RESULT: MAPPED-WALL (vsock validation needs a Linux KVM host with vhost_vsock; not available on this macOS/QEMU-TCG host)"
  exit 64
fi

# 2) On a Linux host: is /dev/vhost-vsock present?
if [ ! -e /dev/vhost-vsock ]; then
  echo "MAPPED-WALL: 'vhost-vsock-pci' exists in this QEMU but /dev/vhost-vsock is absent."
  echo "             Load it with: sudo modprobe vhost_vsock"
  echo "RESULT: MAPPED-WALL (host kernel vhost_vsock module not loaded)"
  exit 64
fi

echo "== host AF_VSOCK listener (CID_HOST=2, port $PORT) =="
# Python's socket.AF_VSOCK is Linux-only; this block only runs past the macOS
# wall above, i.e. on Linux. The listener accepts the guest connection and
# echoes, so the guest's request triggers a reply that round-trips back.
python3 - "$PORT" <<'PY' &
import socket, sys
port = int(sys.argv[1])
try:
    AF_VSOCK = socket.AF_VSOCK
except AttributeError:
    print("host listener: AF_VSOCK unavailable (non-Linux)"); sys.exit(0)
s = socket.socket(AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, port))
s.listen(1)
print("host listener: bound (CID_ANY, %d)" % port, flush=True)
try:
    conn, addr = s.accept()
    print("host listener: accepted from %r" % (addr,), flush=True)
    data = conn.recv(65536)
    conn.sendall(data if data else b"ok")
    conn.close()
except Exception as e:
    print("host listener error: %s" % e, flush=True)
PY
LPID=$!

echo "== boot =="
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -device "vhost-vsock-pci,guest-cid=$GUEST_CID,disable-legacy=on,disable-modern=off" \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

for _ in $(seq 1 240); do
  if grep -q "VSOCK-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "VSOCK-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

kill "$QPID" 2>/dev/null; wait "$QPID" 2>/dev/null
kill "$LPID" 2>/dev/null; wait "$LPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null

echo "== verdict =="
if grep -q "VSOCK-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then
  echo "RESULT: PASS (packet round-tripped on a real vhost-vsock device)"
  exit 0
fi
if grep -q "VSOCK-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see VSOCK-VALIDATE: FAIL line above)"
  exit 3
fi
echo "RESULT: FAIL (no verdict on serial — guest never reported)"
exit 2
