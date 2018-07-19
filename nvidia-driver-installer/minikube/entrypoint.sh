#!/bin/bash
# Copyright 2017 Google Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o pipefail
set -u

set -x
# This driver version works for TITAN/GeForce/Quardro. This doesn't work for Tesla.
NVIDIA_DRIVER_VERSION="${NVIDIA_DRIVER_VERSION:-390.67}"
NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT="https://us.download.nvidia.com/XFree86/Linux-x86_64/${NVIDIA_DRIVER_VERSION}/NVIDIA-Linux-x86_64-${NVIDIA_DRIVER_VERSION}.run"
NVIDIA_DRIVER_DOWNLOAD_URL="${NVIDIA_DRIVER_DOWNLOAD_URL:-$NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT}"
NVIDIA_INSTALL_DIR_HOST="${NVIDIA_INSTALL_DIR_HOST:-/var/lib/nvidia}"
NVIDIA_INSTALL_DIR_CONTAINER="${NVIDIA_INSTALL_DIR_CONTAINER:-/usr/local/nvidia}"
NVIDIA_INSTALLER_RUNFILE="$(basename "${NVIDIA_DRIVER_DOWNLOAD_URL}")"
ROOT_MOUNT_DIR="${ROOT_MOUNT_DIR:-/root}"
CACHE_FILE="${NVIDIA_INSTALL_DIR_CONTAINER}/.cache"
KERNEL_VERSION="$(uname -r)"
MAJOR_KERNEL_VERSION=$(echo $(uname -r) | cut -d "." -f 1)
set +x

fix_kernel_version() {
  PATTERN="^([0-9]+)\.([0-9]+)\.(.+)"
  PATCH_PATTERN="^[0-9]+$"

  if [[ ${KERNEL_VERSION} =~ ${PATTERN} ]]; then
    FALLBACK_VERSION=${BASH_REMATCH[1]}.${BASH_REMATCH[2]}

    # On cdn.kernel.org, the versions are like x.y when z is 0 and x.y.z when z
    # is non-zero. Fix the version returned by 'uname -r' to reflect this
    # reality.
    if [[ ${BASH_REMATCH[3]} == 0 ]]; then
      KERNEL_VERSION=${FALLBACK_VERSION}

    # For a non-standard version like x.y.z-something
    # Try best effort driver installation with headers from version x.y
    elif ! [[ ${BASH_REMATCH[3]} =~ ${PATCH_PATTERN} ]]; then
      KERNEL_VERSION=${FALLBACK_VERSION}
    fi
  fi
  echo "KERNEL_VERSION: ${KERNEL_VERSION}"
}


check_cached_version() {
  echo "Checking cached version"
  if [[ ! -f "${CACHE_FILE}" ]]; then
    echo "Cache file ${CACHE_FILE} not found."
    return 1
  fi

  # Source the cache file and check if the cached driver matches
  # currently running kernel version and requested driver versions.
  . "${CACHE_FILE}"
  if [[ "${KERNEL_VERSION}" == "${CACHE_KERNEL_VERSION}" ]]; then
    if [[ "${NVIDIA_DRIVER_VERSION}" == "${CACHE_NVIDIA_DRIVER_VERSION}" ]]; then
      echo "Found existing driver installation for kernel version ${KERNEL_VERSION} and driver version ${NVIDIA_DRIVER_VERSION}."
      return 0
    fi
  fi
  echo "Cache file ${CACHE_FILE} found but existing versions didn't match."
  return 1
}

update_cached_version() {
  cat >"${CACHE_FILE}"<<__EOF__
CACHE_KERNEL_VERSION=${KERNEL_VERSION}
CACHE_NVIDIA_DRIVER_VERSION=${NVIDIA_DRIVER_VERSION}
__EOF__

  echo "Updated cached version as:"
  cat "${CACHE_FILE}"
}

update_container_ld_cache() {
  echo "Updating container's ld cache..."
  echo "${NVIDIA_INSTALL_DIR_CONTAINER}/lib64" > /etc/ld.so.conf.d/nvidia.conf
  ldconfig
  echo "Updating container's ld cache... DONE."
}

download_kernel_src() {
  echo "Downloading kernel sources..."
  pushd /usr/src
  curl https://cdn.kernel.org/pub/linux/kernel/v${MAJOR_KERNEL_VERSION}.x/linux-${KERNEL_VERSION}.tar.xz > linux-${KERNEL_VERSION}.tar.xz
  tar -xf linux-${KERNEL_VERSION}.tar.xz
  popd
  echo "Downloading kernel sources... DONE."
}

configure_kernel_src() {
  echo "Configuring kernel sources..."
  pushd /usr/src/linux-${KERNEL_VERSION}
  zcat /proc/config.gz > .config
  make olddefconfig
  make modules_prepare
  popd
  echo "Configuring kernel sources... DONE"
}

configure_nvidia_installation_dirs() {
  echo "Configuring installation directories..."
  mkdir -p "${NVIDIA_INSTALL_DIR_CONTAINER}"
  pushd "${NVIDIA_INSTALL_DIR_CONTAINER}"

  # nvidia-installer does not provide an option to configure the
  # installation path of `nvidia-modprobe` utility and always installs it
  # under /usr/bin. The following workaround ensures that
  # `nvidia-modprobe` is accessible outside the installer container
  # filesystem.
  mkdir -p bin bin-workdir
  mount -t overlay -o lowerdir=/usr/bin,upperdir=bin,workdir=bin-workdir none /usr/bin

  # nvidia-installer does not provide an option to configure the
  # installation path of libraries such as libnvidia-ml.so. The following
  # workaround ensures that the libs are accessible from outside the
  # installer container filesystem.
  mkdir -p lib64 lib64-workdir
  mkdir -p /usr/lib/x86_64-linux-gnu
  mount -t overlay -o lowerdir=/usr/lib/x86_64-linux-gnu,upperdir=lib64,workdir=lib64-workdir none /usr/lib/x86_64-linux-gnu

  # nvidia-installer does not provide an option to configure the
  # installation path of driver kernel modules such as nvidia.ko. The following
  # workaround ensures that the modules are accessible from outside the
  # installer container filesystem.
  mkdir -p drivers drivers-workdir
  mkdir -p /lib/modules/${KERNEL_VERSION}/video
  mount -t overlay -o lowerdir=/lib/modules/${KERNEL_VERSION}/video,upperdir=drivers,workdir=drivers-workdir none /lib/modules/${KERNEL_VERSION}/video

  # Populate ld.so.conf to avoid warning messages in nvidia-installer logs.
  update_container_ld_cache

  # Install an exit handler to cleanup the overlayfs mount points.
  trap "{ umount /lib/modules/${KERNEL_VERSION}/video; umount /usr/lib/x86_64-linux-gnu ; umount /usr/bin; }" EXIT
  popd
  echo "Configuring installation directories... DONE."
}

download_nvidia_installer() {
  echo "Downloading Nvidia installer..."
  pushd "${NVIDIA_INSTALL_DIR_CONTAINER}"
  curl -L -S -f "${NVIDIA_DRIVER_DOWNLOAD_URL}" -o "${NVIDIA_INSTALLER_RUNFILE}"
  popd
  echo "Downloading Nvidia installer... DONE."
}

run_nvidia_installer() {
  echo "Running Nvidia installer..."
  pushd "${NVIDIA_INSTALL_DIR_CONTAINER}"
  sh "${NVIDIA_INSTALLER_RUNFILE}" \
    --utility-prefix="${NVIDIA_INSTALL_DIR_CONTAINER}" \
    --opengl-prefix="${NVIDIA_INSTALL_DIR_CONTAINER}" \
    --no-install-compat32-libs \
    --log-file-name="${NVIDIA_INSTALL_DIR_CONTAINER}/nvidia-installer.log" \
    --kernel-source-path=/usr/src/linux-${KERNEL_VERSION} \
    --silent \
    --accept-license
  popd
  echo "Running Nvidia installer... DONE."
}

configure_cached_installation() {
  echo "Configuring cached driver installation..."
  update_container_ld_cache
  if ! lsmod | grep -q -w 'nvidia'; then
    insmod "${NVIDIA_INSTALL_DIR_CONTAINER}/drivers/nvidia.ko"
  fi
  if ! lsmod | grep -q -w 'nvidia_uvm'; then
    insmod "${NVIDIA_INSTALL_DIR_CONTAINER}/drivers/nvidia-uvm.ko"
  fi
  if ! lsmod | grep -q -w 'nvidia_drm'; then
    insmod "${NVIDIA_INSTALL_DIR_CONTAINER}/drivers/nvidia-drm.ko"
  fi
  echo "Configuring cached driver installation... DONE"
}

verify_nvidia_installation() {
  echo "Verifying Nvidia installation..."
  export PATH="${NVIDIA_INSTALL_DIR_CONTAINER}/bin:${PATH}"
  nvidia-smi
  # Create unified memory device file.
  nvidia-modprobe -c0 -u
  echo "Verifying Nvidia installation... DONE."
}

update_host_ld_cache() {
  echo "Updating host's ld cache..."
  echo "${NVIDIA_INSTALL_DIR_HOST}/lib64" >> "${ROOT_MOUNT_DIR}/etc/ld.so.conf"
  ldconfig -r "${ROOT_MOUNT_DIR}"
  echo "Updating host's ld cache... DONE."
}

main() {
  fix_kernel_version
  if check_cached_version; then
    configure_cached_installation
    verify_nvidia_installation
  else
    download_kernel_src
    configure_nvidia_installation_dirs
    download_nvidia_installer
    configure_kernel_src
    run_nvidia_installer
    update_cached_version
    verify_nvidia_installation
  fi
  update_host_ld_cache
}

main "$@"
