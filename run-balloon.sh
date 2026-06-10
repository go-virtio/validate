#!/usr/bin/env bash
# Host harness: build the tamago/amd64 balloon validate ELF, boot the guest
# headless under QEMU (q35, TCG) with a real virtio-balloon-pci device, and
# wait for the guest's inflate/deflate verdict on the serial console.
#
# The guest proof is self-contained: it inflates then deflates the balloon and
# asserts the device CONSUMED each PFN buffer off the used ring (a dead device
# times out). It prints BALLOON-VALIDATE: PASS / FAIL.
#
# HOST-SIDE CROSS-CHECK: a QMP monitor on a unix socket lets us run
# `query-balloon` before and after the guest run, for transparency. NOTE: the
# go-virtio/balloon driver deliberately does NOT write the `actual` field back
# to device config (a documented transport limitation), so QEMU's reported
# balloon "actual" tracks the host-set TARGET, not the guest's guest-initiated
# inflate. We therefore PRINT the host view but do NOT gate the verdict on it —
# the authoritative proof is the guest-side used-ring consumption.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/balloonvalidate.elf"
SERIAL="/tmp/balloonvalidate_serial.log"
QMP="/tmp/balloonvalidate_qmp.sock"
CPU="${CPU:-max}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/balloonvalidate ) || exit 1

rm -f "$SERIAL" "$QMP"

echo "== boot =="
# disable-legacy=on forces a NON-transitional (modern-only) device so its PCI
# device ID is the modern 0x1045 (1af4:1045) that go-virtio/balloon requires.
# Without it QEMU's virtio-balloon-pci defaults to a transitional device whose
# legacy ID the modern-only driver (and the guest's pci.Probe for 0x1045) will
# not bind. A QMP monitor on a unix socket is exposed for the host-side
# query-balloon cross-check.
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -device virtio-balloon-pci,disable-legacy=on,disable-modern=off,id=balloon0 \
  -qmp "unix:$QMP,server=on,wait=off" \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

# qmp runs one QMP command (JSON) against the monitor socket and prints the
# raw reply. It performs the negotiation handshake (qmp_capabilities) first.
qmp() {
  python3 - "$QMP" "$1" <<'PY'
import socket, sys, json, time
sock_path, cmd = sys.argv[1], sys.argv[2]
# Wait for the QMP socket to exist.
for _ in range(80):
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(sock_path)
        break
    except OSError:
        time.sleep(0.05)
else:
    print("qmp: could not connect"); sys.exit(0)
f = s.makefile("rwb")
f.readline()  # greeting
f.write(b'{"execute":"qmp_capabilities"}\n'); f.flush()
f.readline()  # capabilities reply
f.write((cmd + "\n").encode()); f.flush()
print(f.readline().decode().strip())
s.close()
PY
}

# Host view BEFORE the guest has done anything meaningful (best-effort; the
# guest may already be mid-run, that's fine — this is transparency only).
echo "== host query-balloon (before) =="
qmp '{"execute":"query-balloon"}'

# Wait (max ~30s) for the guest to report a PASS/FAIL verdict on serial.
for _ in $(seq 1 120); do
  if grep -q "BALLOON-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "BALLOON-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

# Host view AFTER the guest run, and demonstrate the HOST can change the
# balloon target (this is the host-driven path, distinct from the guest's
# self-initiated inflate which the driver does not report back to config).
echo "== host query-balloon (after guest run) =="
qmp '{"execute":"query-balloon"}'
echo "== host sets balloon target to 1536 MiB (host-driven path) =="
qmp '{"execute":"balloon","arguments":{"value":1610612736}}'
echo "== host query-balloon (after host-driven set) =="
qmp '{"execute":"query-balloon"}'

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null

echo "== verdict =="
if grep -q "BALLOON-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then
  echo "RESULT: PASS (inflate/deflate consumed off the used ring on a real virtio-balloon-pci device)"
  exit 0
fi
if grep -q "BALLOON-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see BALLOON-VALIDATE: FAIL line above)"
  exit 3
fi
echo "RESULT: FAIL (no verdict on serial — guest never reported)"
exit 2
