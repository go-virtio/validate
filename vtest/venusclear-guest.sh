#!/bin/bash
# venusclear-guest.sh — runs INSIDE the Debian arm64 guest (delivered via the
# cloud-init NoCloud seed by run-venus.sh). It installs lavapipe + virgl-server,
# starts `virgl_test_server --venus` backed by lavapipe, drives the Venus
# CLEAR-IMAGE program (/root/venusclear), and dumps EVERYTHING — including the
# server (vkr) stderr, the per-command head-advance trace, and the decoded
# handles/VkResults — to the serial console wrapped in unique markers so the
# host orchestrator can parse the verdict regardless of the getty race.
#
# Task-2 note: the host-behaviour evidence is the vkr server.log AROUND the
# clear (image created + CmdClearColorImage dispatched + QueueSubmit completed
# with no CS/validation error). We print the whole server.log after the run.
set -x
R=/root/RESULTS.txt
exec > "$R" 2>&1
echo "=========== VENUS CLEAR-IMAGE START ==========="
echo "nameserver 10.0.2.3" > /etc/resolv.conf
echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99force-ipv4
echo 'precedence ::ffff:0:0/96  100' >> /etc/gai.conf
export DEBIAN_FRONTEND=noninteractive

echo "--- apt update ---"
timeout 200 apt-get -o Acquire::ForceIPv4=true -o Acquire::http::Timeout=20 -o Acquire::Retries=2 update
echo "APT-UPDATE-EXIT=$?"
echo "--- install virgl-server + lavapipe (mesa-vulkan-drivers) + vulkan-tools ---"
timeout 400 apt-get install -y --no-install-recommends virgl-server mesa-vulkan-drivers libvulkan1 vulkan-tools
echo "APT-INSTALL-EXIT=$?"

SRV=$(command -v virgl_test_server || find / -name 'virgl_test_server*' -type f 2>/dev/null | head -1)
echo "SRV=$SRV"

echo "--- lavapipe ICD present? ---"
ls -la /usr/share/vulkan/icd.d/ 2>&1
LVP_ICD=$(ls /usr/share/vulkan/icd.d/lvp_icd*.json 2>/dev/null | head -1)
echo "LVP_ICD=$LVP_ICD"

echo "--- vulkaninfo (which device backs Vulkan?) ---"
VK_ICD_FILENAMES="$LVP_ICD" vulkaninfo --summary 2>&1 | head -40 || echo "no vulkaninfo"

chmod +x /root/venusclear /root/vtest.test 2>/dev/null

# Force the host Venus renderer onto lavapipe (software Vulkan).
export VK_ICD_FILENAMES="$LVP_ICD"
export VK_DRIVER_FILES="$LVP_ICD"
export GALLIUM_DRIVER=llvmpipe LIBGL_ALWAYS_SOFTWARE=1
# Verbose vkr logging so the server.log carries the renderer's own confirmation
# of the image creation + clear dispatch (host-behaviour evidence for Task 2).
export VN_DEBUG=result
export VIRGL_LOG_LEVEL=debug
# IMPORTANT: the server enables timeline support only when
# getenv("VIRGL_DISABLE_MT") == NULL (vtest_get_param: `if (!getenv(...))`).
# ANY value, including "0", disables it. Venus REQUIRES max_timeline_count>0,
# so the var must be fully UNSET.
unset VIRGL_DISABLE_MT

start_venus_server() {
  local desc="$1"; shift
  rm -f /tmp/.virgl_test
  echo "### TRY VENUS SERVER: $desc :: $*"
  ( "$@" ) > /root/server.log 2>&1 &
  SRVPID=$!
  for i in $(seq 1 15); do [ -S /tmp/.virgl_test ] && break; sleep 1; done
  sleep 1
  echo "--- server.log (startup) ---"; cat /root/server.log
  if [ -S /tmp/.virgl_test ] && kill -0 "$SRVPID" 2>/dev/null; then
    return 0
  fi
  echo "server did not come up"
  return 1
}

OK=0
start_venus_server "venus + egl-surfaceless" env EGL_PLATFORM=surfaceless "$SRV" --venus --use-egl-surfaceless && OK=1
if [ $OK -eq 0 ]; then
  start_venus_server "venus only" "$SRV" --venus && OK=1
fi
echo "VENUS-SERVER-UP=$OK"

if [ $OK -eq 1 ]; then
  echo "=========== RUN VENUS CLEAR-IMAGE ==========="
  /root/venusclear -sock /tmp/.virgl_test -timeout 8s 2>&1; echo "VENUS-CLEAR-EXIT=$?"
  echo "--- server.log after clear (vkr host-behaviour evidence / THE WALL) ---"
  cat /root/server.log 2>&1
fi

echo "=========== RUN LINUX-TAGGED UNIT TESTS (transport + ring + clear helpers) ==========="
/root/vtest.test -test.v 2>&1 | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)' | tail -80
echo "UNIT-TEST-EXIT=${PIPESTATUS[0]}"

echo "=========== VENUS CLEAR-IMAGE DONE ==========="
{
echo ""
echo "@@@@@@@@@@ RESULTS-BEGIN @@@@@@@@@@"
cat "$R"
echo "@@@@@@@@@@ RESULTS-END @@@@@@@@@@"
echo ""
} > /dev/console 2>&1
sync
sleep 2
poweroff
