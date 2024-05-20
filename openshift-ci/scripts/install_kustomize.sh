#!/bin/bash

set -e

# Function to get the latest Kustomize version from GitHub API
get_latest_version() {
  curl -s "https://api.github.com/repos/kubernetes-sigs/kustomize/releases/latest" |
    grep '"tag_name"' |
    sed -E 's/.*"([^"]+)".*/\1/'
}

# Function to detect the operating system
detect_os() {
  case "$(uname -s)" in
    Linux*)     os="linux";;
    Darwin*)    os="darwin";;
    CYGWIN*|MINGW*|MSYS_NT*) os="windows";;
    *)          echo "Unsupported OS: $(uname -s)"; exit 1;;
  esac
  echo "${os}"
}

# Function to detect the architecture
detect_arch() {
  case "$(uname -m)" in
    x86_64)     arch="amd64";;
    armv7l)     arch="arm";;
    aarch64)    arch="arm64";;
    *)          echo "Unsupported architecture: $(uname -m)"; exit 1;;
  esac
  echo "${arch}"
}

# Function to download and install Kustomize
install_kustomize() {
  local version=$1
  local os=$2
  local arch=$3

  local url="https://github.com/kubernetes-sigs/kustomize/releases/download/${version}/kustomize_${version}_${os}_${arch}.tar.gz"

  echo "Downloading Kustomize from ${url}..."
  curl -LO "${url}"

  echo "Extracting Kustomize..."
  tar -zxvf "kustomize_${version}_${os}_${arch}.tar.gz"

  echo "Installing Kustomize..."
  chmod +x kustomize
  sudo mv kustomize /usr/local/bin/kustomize

  echo "Kustomize installed successfully!"
}

# Main script execution
main() {
  echo "Detecting OS and architecture..."
  os=$(detect_os)
  arch=$(detect_arch)

  echo "OS: ${os}"
  echo "Architecture: ${arch}"

  echo "Fetching the latest Kustomize version..."
  latest_version=$(get_latest_version)
  echo "Latest Kustomize version is ${latest_version}"

  install_kustomize "${latest_version}" "${os}" "${arch}"

  echo "Kustomize installation completed."
}

# Run the main function
main