#!/usr/bin/env bash
# Host harness: build the tamago/amd64 blk validate ELF, create an 8 MiB
# scratch raw disk, boot the guest headless under QEMU (q35, TCG) with a
# virtio-blk-pci device backed by that disk, and wait for the guest's
# in-guest write/read-back verdict on the serial console.
#
# Unlike the gpu harness this needs NO host-side observation: the guest writes
# a deterministic LBA-stamped pattern, Flushes, reads it back, and asserts
# byte-equality itself, printing BLK-VALIDATE: PASS / FAIL. We exit non-zero on
# FAIL (or if no verdict appears).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
TAMAGO="${TAMAGO:-/Users/david_delavennat/Documents/VCS/GIT/github.com/tannevaled/tamago-go/bin/go}"
ELF="$HERE/blkvalidate.elf"
IMG="${IMG:-/tmp/blkvalidate.img}"
SERIAL="/tmp/blkvalidate_serial.log"
CPU="${CPU:-max}"

echo "== build =="
( cd "$HERE" && GOWORK=off GOOS=tamago GOARCH=amd64 \
  GOOSPKG=github.com/usbarmory/tamago \
  "$TAMAGO" build -ldflags "-T 0x10010000 -R 0x1000" -o "$ELF" ./cmd/blkvalidate ) || exit 1

echo "== scratch disk =="
rm -f "$IMG" "$SERIAL"
# 8 MiB raw image == 16384 sectors of 512 bytes (the capacity the guest checks).
if command -v qemu-img >/dev/null 2>&1; then
  qemu-img create -f raw "$IMG" 8M >/dev/null || exit 1
else
  dd if=/dev/zero of="$IMG" bs=1m count=8 2>/dev/null || \
  dd if=/dev/zero of="$IMG" bs=1M count=8 2>/dev/null || exit 1
fi
ls -l "$IMG"

echo "== boot =="
# disable-legacy=on forces a NON-transitional (modern-only) device so its PCI
# device ID is the modern 0x1042 (1af4:1042) that go-virtio/blk requires.
# Without it, QEMU's virtio-blk-pci defaults to a transitional device that
# advertises the legacy ID 0x1001 in PCI config space, which the modern-only
# driver (and the guest's pci.Probe for 0x1042) will not bind.
qemu-system-x86_64 -M q35 -accel tcg -cpu "$CPU" -m 2G \
  -display none -no-reboot \
  -device virtio-blk-pci,drive=d0,disable-legacy=on,disable-modern=off \
  -drive "if=none,id=d0,file=$IMG,format=raw" \
  -serial "file:$SERIAL" \
  -kernel "$ELF" &
QPID=$!

# Wait (max ~30s) for the guest to report a PASS/FAIL verdict on serial.
for _ in $(seq 1 120); do
  if grep -q "BLK-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then break; fi
  if grep -q "BLK-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then break; fi
  if ! kill -0 "$QPID" 2>/dev/null; then break; fi
  sleep 0.25
done

kill "$QPID" 2>/dev/null
wait "$QPID" 2>/dev/null

echo "== serial =="
cat "$SERIAL" 2>/dev/null

echo "== verdict =="
if grep -q "BLK-VALIDATE: PASS" "$SERIAL" 2>/dev/null; then
  echo "RESULT: PASS (write/read-back byte-equal on a real virtio-blk-pci device)"
  exit 0
fi
if grep -q "BLK-VALIDATE: FAIL" "$SERIAL" 2>/dev/null; then
  echo "RESULT: FAIL (see BLK-VALIDATE: FAIL line above)"
  exit 3
fi
echo "RESULT: FAIL (no verdict on serial — guest never reported)"
exit 2
