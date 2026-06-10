#!/usr/bin/env bash
# Host harness: build the tamago/amd64 rng validate ELF, boot the guest
# headless under QEMU (q35, TCG) with a real virtio-rng-pci device, and wait
# for the guest's entropy verdict on the serial console.
#
# This needs NO host-side observation: the guest reads entropy from the device
# and asserts itself that (a) a read returns the requested length, (b) two
# successive reads differ, and (c) a large sample is non-zero, covers most of
# the 256 byte values, and passes a chi-square uniformity bound. It prints
# RNG-VALIDATE: PASS / FAIL. We exit non-zero on FAIL (or if no verdict
# appears).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/rngvalidate.elf"
SERIAL="/tmp/rngvalidate_serial.log"
CPU="${CPU:-max}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/rngvalidate ) || exit 1

rm -f "$SERIAL"

echo "== boot =="
# disable-legacy=on forces a NON-transitional (modern-only) device so its PCI
# device ID is the modern 0x1044 (1af4:1044) that go-virtio/rng requires.
# Without it QEMU's virtio-rng-pci defaults to a transitional device advertising
# the legacy ID, which the modern-only driver (and the guest's pci.Probe for
# 0x1044) will not bind. The default rng backend (rng-builtin) supplies real
# entropy; we add an explicit rng-random object on /dev/urandom to be sure.
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -object rng-random,filename=/dev/urandom,id=rng0 \
  -device virtio-rng-pci,rng=rng0,disable-legacy=on,disable-modern=off \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

# Wait (max ~30s) for the guest to report a PASS/FAIL verdict on serial.
for _ in $(seq 1 120); do
  if grep -q "RNG-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "RNG-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null

echo "== verdict =="
if grep -q "RNG-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then
  echo "RESULT: PASS (entropy delivered + distinct + uniform on a real virtio-rng-pci device)"
  exit 0
fi
if grep -q "RNG-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see RNG-VALIDATE: FAIL line above)"
  exit 3
fi
echo "RESULT: FAIL (no verdict on serial — guest never reported)"
exit 2
