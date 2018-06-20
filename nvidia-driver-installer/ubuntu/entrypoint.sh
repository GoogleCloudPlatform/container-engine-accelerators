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
NVIDIA_DRIVER_VERSION="${NVIDIA_DRIVER_VERSION:-384.111}"
NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT="https://us.download.nvidia.com/tesla/${NVIDIA_DRIVER_VERSION}/NVIDIA-Linux-x86_64-${NVIDIA_DRIVER_VERSION}.run"
NVIDIA_DRIVER_DOWNLOAD_URL="${NVIDIA_DRIVER_DOWNLOAD_URL:-$NVIDIA_DRIVER_DOWNLOAD_URL_DEFAULT}"
NVIDIA_INSTALL_DIR_HOST="${NVIDIA_INSTALL_DIR_HOST:-/var/lib/nvidia}"
NVIDIA_INSTALL_DIR_CONTAINER="${NVIDIA_INSTALL_DIR_CONTAINER:-/usr/local/nvidia}"
NVIDIA_INSTALLER_RUNFILE="$(basename "${NVIDIA_DRIVER_DOWNLOAD_URL}")"
ROOT_MOUNT_DIR="${ROOT_MOUNT_DIR:-/root}"
set +x

RETCODE_SUCCESS=0
RETCODE_ERROR=1
RETRY_COUNT=5

remove_nouveau_kernel_module() {
  # if nouveau kernel module is loaded install will fail
  # so this will check if the module is loaded and and remove if needed
  echo "Checking if nouveau kernel module is loaded"
  lsmod=$(lsmod | { grep nouveau || true ; }  | wc -l)
  echo "Removing nouveau if needed"
  if [ "$lsmod" != "0" ] ; then rmmod nouveau ; fi
}


check_if_nvidia_module_is_installed() {
  # use modinfo to check if module is installed
  modinfo=$(modinfo nvidia | { grep ERROR || true ; }  | wc -l)
  if [ "$modinfo" == "0" ] ;
  then
    echo "Nvidia module found"
    return 0
  else
    echo "Nvidia module not found"
    return 1
  fi
}

load_nvidia_module() {
  # load nvidia_module
  # do lsmod of nvidia module
  echo "Checking if module is already loaded"
  lsmod=$(lsmod | { grep nvidia || true ; }  | wc -l)
  if [ "$lsmod" == "0" ] ;
  then
    echo "Loading nvida module"
    modprobe nvidia
  fi
}

report_if_module_version_is_correct() {
  # get version of module. Automatic update of version could be implemented
  # but removing module can be trouble to automate
  version=$(modinfo nvidia | grep ^version | cut -d ' ' -f 9)
  if [ "$version" == "$NVIDIA_DRIVER_VERSION" ]
  then
    echo "Nvidia driver version is correct and is: $version"
  else
    echo "Nvidia driver version should be $NVIDIA_DRIVER_VERSION but is $version"
    echo "Manual update needed"
  fi
}

update_container_ld_cache() {
  echo "Updating container's ld cache..."
  echo "${NVIDIA_INSTALL_DIR_CONTAINER}/lib64" > /etc/ld.so.conf.d/nvidia.conf
  ldconfig
  echo "Updating container's ld cache... DONE."
}

download_kernel_src() {
  echo "Downloading kernel sources..."
  apt-get update && apt-get install -y linux-headers-$(uname -r)
  echo "Downloading kernel sources... DONE."
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
  mkdir -p /lib/modules/"$(uname -r)"/video
  mount -t overlay -o lowerdir=/lib/modules/"$(uname -r)"/video,upperdir=drivers,workdir=drivers-workdir none /lib/modules/"$(uname -r)"/video

  # Populate ld.so.conf to avoid warning messages in nvidia-installer logs.
  update_container_ld_cache

  # Install an exit handler to cleanup the overlayfs mount points.
  trap "{ umount /lib/modules/\"$(uname -r)\"/video; umount /usr/lib/x86_64-linux-gnu ; umount /usr/bin; }" EXIT
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
    --no-drm \
    --silent \
    --accept-license
  popd
  echo "Running Nvidia installer... DONE."
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
  remove_nouveau_kernel_module
  if check_if_nvidia_module_is_installed
  then
    load_nvidia_module
    report_if_module_version_is_correct
  else
    download_kernel_src
    configure_nvidia_installation_dirs
    download_nvidia_installer
    run_nvidia_installer
    verify_nvidia_installation
    update_host_ld_cache
  fi
}

main "$@"
