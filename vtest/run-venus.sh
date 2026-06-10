#!/usr/bin/env bash
# run-venus.sh — committed, reproducible Debian-Venus harness for the Venus
# CLEAR-IMAGE program. It is the formalized replacement for the ad-hoc
# /tmp/debianvm scratch + xorriso flow: the cloud-init NoCloud seed is built in
# PURE GO via ../cmd/mkseed (no xorriso, no external C tool), and the whole
# harness is source-controlled next to the driver it exercises.
#
# WHAT IT DOES
#   1. cross-compiles, for linux/arm64 (CGO=0):
#        - cmd/venusclear        -> the live Venus clear-image driver, and
#        - the package test binary (vtest.test, linux-tagged unit tests);
#   2. assembles a #cloud-config user-data that write_files-embeds those two
#      binaries (base64) plus the committed venusclear-guest.sh runner, and
#      runcmds the runner;
#   3. builds seed.iso from that user-data with `go run ../cmd/mkseed`
#      (weft-cidata, pure Go) — NOT xorriso;
#   4. boots the Debian arm64 cloud image headless under QEMU (hvf) with the
#      seed attached, waiting for the guest to print its RESULTS markers to the
#      serial console;
#   5. extracts the RESULTS block and parses the PASS/FAIL verdict.
#
# All scratch lives under $WORKDIR (default ./.venus-run, gitignored), NOT /tmp.
#
# Tunables (env):
#   WORKDIR     scratch dir for the disk/seed/serial log (default ./.venus-run)
#   BASE_IMG    Debian arm64 genericcloud qcow2 (downloaded if absent)
#   FIRMWARE    edk2 aarch64 code fd (default the Homebrew path)
#   MEM/SMP     guest memory / vcpus (default 3G / 2)
#   BOOT_TIMEOUT seconds to wait for the RESULTS block (default 900)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKDIR="${WORKDIR:-$HERE/.venus-run}"
FIRMWARE="${FIRMWARE:-/opt/homebrew/share/qemu/edk2-aarch64-code.fd}"
MEM="${MEM:-3G}"
SMP="${SMP:-2}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-900}"
DEBIAN_VER="${DEBIAN_VER:-12}"
BASE_URL="https://cloud.debian.org/images/cloud/bookworm/latest/debian-${DEBIAN_VER}-genericcloud-arm64.qcow2"
BASE_IMG="${BASE_IMG:-$WORKDIR/debian-${DEBIAN_VER}-genericcloud-arm64.qcow2}"
INSTANCE_ID="${INSTANCE_ID:-venus-clear-$(date +%s)}"

mkdir -p "$WORKDIR"
echo "==> workdir: $WORKDIR"

# --- 1. cross-compile the driver + the linux unit-test binary ---------------
echo "==> cross-compiling venusclear + vtest.test (linux/arm64, CGO=0)"
( cd "$HERE" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -o "$WORKDIR/venusclear" ./cmd/venusclear )
( cd "$HERE" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go test -c -o "$WORKDIR/vtest.test" ./ )

# --- 2. assemble the #cloud-config user-data --------------------------------
echo "==> assembling cloud-init user-data (binaries base64-embedded)"
UD="$WORKDIR/user-data"
b64() { base64 < "$1" | sed 's/^/      /'; }
{
  echo "#cloud-config"
  echo "write_files:"
  echo "  - path: /root/venusclear-guest.sh"
  echo "    permissions: '0755'"
  echo "    encoding: b64"
  echo "    content: |"
  b64 "$HERE/venusclear-guest.sh"
  echo "  - path: /root/venusclear"
  echo "    permissions: '0755'"
  echo "    encoding: b64"
  echo "    content: |"
  b64 "$WORKDIR/venusclear"
  echo "  - path: /root/vtest.test"
  echo "    permissions: '0755'"
  echo "    encoding: b64"
  echo "    content: |"
  b64 "$WORKDIR/vtest.test"
  echo "runcmd:"
  echo "  - [ bash, /root/venusclear-guest.sh ]"
} > "$UD"
echo "    user-data: $(wc -c < "$UD") bytes"

# --- 3. build the NoCloud seed in PURE GO (no xorriso) ----------------------
echo "==> building seed.iso via go run ../cmd/mkseed (pure Go, no xorriso)"
SEED="$WORKDIR/seed.iso"
( cd "$HERE/.." && GOWORK=off go run ./cmd/mkseed \
    -instance-id "$INSTANCE_ID" -hostname venusdeb \
    -user-data "$UD" -out "$SEED" )

# --- 4. boot the Debian cloud image headless under QEMU ---------------------
if [ ! -f "$BASE_IMG" ]; then
  echo "==> base image absent; downloading $BASE_URL"
  curl -fL --retry 3 -o "$BASE_IMG" "$BASE_URL"
fi
DISK="$WORKDIR/disk.qcow2"
echo "==> preparing overlay disk (base stays pristine)"
qemu-img create -f qcow2 -F qcow2 -b "$BASE_IMG" "$DISK" >/dev/null
# Per-boot UEFI varstore (writable copy of the firmware vars).
VARS="$WORKDIR/varstore.fd"
if [ ! -f "$VARS" ]; then
  # 64 MiB blank varstore, matching the edk2 aarch64 layout.
  dd if=/dev/zero of="$VARS" bs=1M count=64 2>/dev/null
fi

SERIAL="$WORKDIR/serial.log"
: > "$SERIAL"
echo "==> booting QEMU headless (waiting up to ${BOOT_TIMEOUT}s for RESULTS)"

ACCEL=( -accel hvf -cpu host )
# Fall back to TCG if hvf is unavailable.
if ! qemu-system-aarch64 -accel help 2>/dev/null | grep -q hvf; then
  ACCEL=( -cpu cortex-a72 )
fi

qemu-system-aarch64 \
  -M virt "${ACCEL[@]}" -smp "$SMP" -m "$MEM" \
  -drive if=pflash,format=raw,readonly=on,file="$FIRMWARE" \
  -drive if=pflash,format=raw,file="$VARS" \
  -drive if=virtio,format=qcow2,file="$DISK" \
  -drive if=virtio,format=raw,file="$SEED" \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  -nographic -serial "file:$SERIAL" -no-reboot &
QEMU_PID=$!

# --- 5. wait for the RESULTS block, then parse the verdict ------------------
deadline=$(( $(date +%s) + BOOT_TIMEOUT ))
while kill -0 "$QEMU_PID" 2>/dev/null; do
  if grep -q "@@@@@@@@@@ RESULTS-END @@@@@@@@@@" "$SERIAL" 2>/dev/null; then
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "!! timeout waiting for RESULTS; killing QEMU" >&2
    kill "$QEMU_PID" 2>/dev/null || true
    break
  fi
  sleep 3
done
wait "$QEMU_PID" 2>/dev/null || true

echo "==> RESULTS block:"
awk '/RESULTS-BEGIN/{f=1} f{print} /RESULTS-END/{f=0}' "$SERIAL" || true

echo "==> verdict:"
if grep -q "^PASS: VENUS clear-image" "$SERIAL"; then
  grep "^PASS: VENUS clear-image" "$SERIAL"
  echo "RUN-VENUS: PASS"
  exit 0
elif grep -qE "VENUS-CLEAR-WALL|^FAIL: VENUS clear-image" "$SERIAL"; then
  grep -E "VENUS-CLEAR-WALL|^FAIL: VENUS clear-image" "$SERIAL" | head -5
  echo "RUN-VENUS: FAIL (see $SERIAL and the server.log block above for the host stderr / next wall)"
  exit 2
else
  echo "RUN-VENUS: INCONCLUSIVE (no verdict marker; inspect $SERIAL)"
  exit 3
fi
