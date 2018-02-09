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

KERNEL_SRC_URL="https://cdn.kernel.org/pub/linux/kernel/v4.x"
KERNEL_SRC_ARCHIVE="linux-$(uname -r | cut -d- -f1).tar.xz"
KERNEL_SRC_DIR="${KERNEL_SRC_DIR:-/build/usr/src/linux}"
ROOT_OS_RELEASE="${ROOT_OS_RELEASE:-/root/etc/os-release}"
NVIDIA_DRIVER_VERSION="${NVIDIA_DRIVER_VERSION:-384.111}"
NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT="https://us.download.nvidia.com/tesla/${NVIDIA_DRIVER_VERSION}/NVIDIA-Linux-x86_64-${NVIDIA_DRIVER_VERSION}.run"
NVIDIA_DRIVER_DOWNLOAD_URL="${NVIDIA_DRIVER_DOWNLOAD_URL:-$NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT}"
NVIDIA_INSTALL_DIR_HOST="${NVIDIA_INSTALL_DIR_HOST:-/opt/nvidia}"
NVIDIA_INSTALL_DIR_CONTAINER="${NVIDIA_INSTALL_DIR_CONTAINER:-/usr/local/nvidia}"
NVIDIA_INSTALLER_RUNFILE="$(basename "${NVIDIA_DRIVER_DOWNLOAD_URL}")"
ROOT_MOUNT_DIR="${ROOT_MOUNT_DIR:-/root}"
CACHE_FILE="${NVIDIA_INSTALL_DIR_CONTAINER}/.cache"
set +x

RETCODE_SUCCESS=0
RETCODE_ERROR=1
RETRY_COUNT=5

_log() {
  local -r prefix="$1"
  shift
  echo "[${prefix}$(date -u "+%Y-%m-%d %H:%M:%S %Z")] ""$*" >&2
}

info() {
  _log "INFO    " "$*"
}

warn() {
  _log "WARNING " "$*"
}

error() {
  _log "ERROR   " "$*"
}

load_etc_os_release() {
  if [[ ! -f "${ROOT_OS_RELEASE}" ]]; then
    error "File ${ROOT_OS_RELEASE} not found, /etc/os-release from Container Linux host must be mounted."
    exit ${RETCODE_ERROR}
  fi
  . "${ROOT_OS_RELEASE}"
  info "Running on Container Linux version id ${VERSION_ID}"
}

check_cached_version() {
  info "Checking cached version"
  if [[ ! -f "${CACHE_FILE}" ]]; then
    info "Cache file ${CACHE_FILE} not found."
    return ${RETCODE_ERROR}
  fi

  # Source the cache file and check if the cached driver matches
  # currently running image build and driver versions.
  . "${CACHE_FILE}"
  if [[ "${VERSION_ID}" == "${CACHE_VERSION_ID}" ]]; then
    if [[ "${NVIDIA_DRIVER_VERSION}" == \
          "${CACHE_NVIDIA_DRIVER_VERSION}" ]]; then
      info "Found existing driver installation for image version ${BUILD_ID} \
          and driver version ${NVIDIA_DRIVER_VERSION}."
      return ${RETCODE_SUCCESS}
    fi
  fi
  return ${RETCODE_ERROR}
}

update_cached_version() {
  cat >"${CACHE_FILE}"<<__EOF__
CACHE_VERSION_ID=${VERSION_ID}
CACHE_NVIDIA_DRIVER_VERSION=${NVIDIA_DRIVER_VERSION}
__EOF__

  info "Updated cached version as:"
  cat "${CACHE_FILE}"
}

update_container_ld_cache() {
  info "Updating container's ld cache"
  echo "${NVIDIA_INSTALL_DIR_CONTAINER}/lib64" > /etc/ld.so.conf.d/nvidia.conf
  ldconfig
}

download_kernel_src_archive() {
  local -r download_url="$1"
  info "Kernel source archive download URL: ${download_url}"
  mkdir -p "${KERNEL_SRC_DIR}"
  pushd "${KERNEL_SRC_DIR}"
  local attempts=0
  until time curl -sfS "${download_url}" -o "${KERNEL_SRC_ARCHIVE}"; do
    attempts=$(( ${attempts} + 1 ))
    if (( "${attempts}" >= "${RETRY_COUNT}" )); then
      error "Could not download kernel sources from ${download_url}, giving up."
      return ${RETCODE_ERROR}
    fi
    warn "Error fetching kernel source archive from ${download_url}, retrying"
    sleep 1
  done
  popd
}

download_kernel_src_from_git_repo() {
  # KERNEL_COMMIT_ID comes from /root/etc/os-release file.
  local -r download_url="${KERNEL_SRC_URL}/${KERNEL_SRC_ARCHIVE}"
  download_kernel_src_archive "${download_url}"
}

download_kernel_src() {
  if [[ -z "$(ls -A "${KERNEL_SRC_DIR}")" ]]; then
    info "Kernel sources not found locally, downloading"
    mkdir -p "${KERNEL_SRC_DIR}"
    if ! download_kernel_src_from_git_repo; then
        return ${RETCODE_ERROR}
    fi
  fi
  pushd "${KERNEL_SRC_DIR}"
  tar xf "${KERNEL_SRC_ARCHIVE}" --strip-components=1
  popd
}

configure_kernel_src() {
  info "Configuring kernel sources"
  pushd "${KERNEL_SRC_DIR}"
  zcat /proc/config.gz > .config
  make olddefconfig
  make modules_prepare

  # TODO: Figure out why the kernel magic version hack is required.
  local kernel_version_uname="$(uname -r)"
  local kernel_version_src="$(cat include/generated/utsrelease.h | awk '{ print $3 }' | tr -d '"')"
  if [[ "${kernel_version_uname}" != "${kernel_version_src}" ]]; then
    info "Modifying kernel version magic string in source files"
    sed -i "s|${kernel_version_src}|${kernel_version_uname}|g" "include/generated/utsrelease.h"
  fi
  popd
}

configure_nvidia_installation_dirs() {
  info "Configuring installation directories"
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
  mkdir -p /lib/modules/"$(uname -r)"/video
  mount -t overlay -o lowerdir=/lib/modules/"$(uname -r)"/video,upperdir=drivers,workdir=drivers-workdir none /lib/modules/"$(uname -r)"/video

  # Populate ld.so.conf to avoid warning messages in nvidia-installer logs.
  update_container_ld_cache

  # Install an exit handler to cleanup the overlayfs mount points.
  trap "{ umount /lib/modules/\"$(uname -r)\"/video; umount /usr/lib/x86_64-linux-gnu ; umount /usr/bin; }" EXIT
  popd
}

download_nvidia_installer() {
  info "Downloading Nvidia installer ... "
  pushd "${NVIDIA_INSTALL_DIR_CONTAINER}"
  curl -L -sS "${NVIDIA_DRIVER_DOWNLOAD_URL}" -o "${NVIDIA_INSTALLER_RUNFILE}"
  popd
}

run_nvidia_installer() {
  info "Running Nvidia installer"
  pushd "${NVIDIA_INSTALL_DIR_CONTAINER}"
  sh "${NVIDIA_INSTALLER_RUNFILE}" \
    --kernel-source-path="${KERNEL_SRC_DIR}" \
    --utility-prefix="${NVIDIA_INSTALL_DIR_CONTAINER}" \
    --opengl-prefix="${NVIDIA_INSTALL_DIR_CONTAINER}" \
    --no-install-compat32-libs \
    --log-file-name="${NVIDIA_INSTALL_DIR_CONTAINER}/nvidia-installer.log" \
    --silent \
    --accept-license
  popd
}

configure_cached_installation() {
  info "Configuring cached driver installation"
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
}

verify_nvidia_installation() {
  info "Verifying Nvidia installation"
  export PATH="${NVIDIA_INSTALL_DIR_CONTAINER}/bin:${PATH}"
  nvidia-smi
  # Create unified memory device file.
  nvidia-modprobe -c0 -u

  # TODO: Add support for enabling persistence mode.
}

update_host_ld_cache() {
  info "Updating host's ld cache"
  mkdir -p ${ROOT_MOUNT_DIR}/etc/ld.so.conf.d
  echo "${NVIDIA_INSTALL_DIR_HOST}/lib64" > "${ROOT_MOUNT_DIR}/etc/ld.so.conf.d/nvidia.conf"
  ldconfig -r "${ROOT_MOUNT_DIR}"
}

main() {
  load_etc_os_release
  if check_cached_version; then
    configure_cached_installation
    verify_nvidia_installation
    info "Found cached version, NOT building the drivers."
  else
    info "Did not find cached version, building the drivers..."
    download_kernel_src
    configure_nvidia_installation_dirs
    download_nvidia_installer
    configure_kernel_src
    run_nvidia_installer
    update_cached_version
    verify_nvidia_installation
    info "Finished installing the drivers."
  fi
  update_host_ld_cache
}

main "$@"
