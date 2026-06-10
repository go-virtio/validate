#!/usr/bin/env bash
# Host harness: build the tamago/amd64 validate ELF, boot it headless under
# QEMU (q35, TCG) with a virtio-gpu-pci device, wait for the guest's "DONE"
# line on the serial console, then issue `screendump` over the QEMU monitor
# and validate the resulting PPM is not a uniform frame.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/validate.elf"
PPM="${PPM:-/tmp/validate_cube.ppm}"
SERIAL="/tmp/validate_serial.log"
MON="/tmp/validate_mon.sock"
CPU="${CPU:-max}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" . ) || exit 1

rm -f "$PPM" "$SERIAL" "$MON"

echo "== boot =="
# -vga none makes the virtio-gpu the only display device, so the guest's
# scanout is what `screendump <ppm> vgpu 0` captures (otherwise the q35
# default VGA is the primary head and the dump is blank text mode).
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot -vga none \
  -device virtio-gpu-pci,id=vgpu \
  -serial "file:$SERIAL" \
  -monitor "unix:$MON,server,nowait" \
  -kernel "$ELF" &
QPID=$!

# Wait (max ~30s) for the guest to report DONE on serial.
for _ in $(seq 1 120); do
  if grep -q "VALIDATE: DONE" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

echo "== serial =="
cat "$SERIAL" 2>/dev/null

if grep -q "VALIDATE: DONE" "$SERIAL" 2>/dev/null; then
  echo "== screendump =="
  # Talk to the QEMU monitor over the unix socket via python (portable).
  python3 - "$MON" "$PPM" <<'PY'
import socket, sys, time
sock, ppm = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(sock)
time.sleep(0.3)
s.recv(65536)  # banner
s.sendall(("screendump %s vgpu 0\n" % ppm).encode())
time.sleep(0.6)
try:
    print(s.recv(65536).decode(errors="replace").strip())
except Exception as e:
    print("monitor read err:", e)
s.close()
PY
fi

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null

echo "== validate PPM =="
if [ ! -f "$PPM" ]; then
  echo "NO PPM PRODUCED"
  exit 2
fi
python3 - "$PPM" <<'PY'
import sys
path = sys.argv[1]
with open(path, "rb") as f:
    data = f.read()
# Parse minimal binary PPM (P6).
assert data[:2] == b"P6", "not a P6 PPM: %r" % data[:8]
idx = 2
fields = []
while len(fields) < 3:
    while idx < len(data) and data[idx] in b" \t\r\n":
        idx += 1
    if data[idx:idx+1] == b"#":
        while idx < len(data) and data[idx] not in b"\r\n":
            idx += 1
        continue
    start = idx
    while idx < len(data) and data[idx] not in b" \t\r\n":
        idx += 1
    fields.append(int(data[start:idx]))
w, h, maxv = fields
idx += 1  # single whitespace after maxval
pix = data[idx:idx + w*h*3]
colors = {}
for i in range(0, len(pix), 3):
    c = pix[i:i+3]
    colors[c] = colors.get(c, 0) + 1
distinct = len(colors)
top = sorted(colors.items(), key=lambda kv: -kv[1])[:6]
dominant = top[0][1] / (w*h) if w*h else 1.0
print("PPM %dx%d maxval=%d distinct_colors=%d dominant_fraction=%.3f"
      % (w, h, maxv, distinct, dominant))
print("top colors (RGB:count):",
      ", ".join("%d,%d,%d:%d" % (c[0], c[1], c[2], n) for c, n in top))
# A rendered cube must be NON-uniform: many colors, no single color covering
# the whole frame.
if distinct >= 4 and dominant < 0.98:
    print("RESULT: PASS (non-uniform frame consistent with a rendered cube)")
    sys.exit(0)
print("RESULT: FAIL (frame is effectively uniform)")
sys.exit(3)
PY
