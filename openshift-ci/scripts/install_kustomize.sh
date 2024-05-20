#!/bin/bash

set -e
version="v3.2.3"

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
  os=$1
  arch=$2

  local url="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F$version/kustomize_kustomize.v3.2.3_$os_$arch"

  echo "Downloading Kustomize from ${url}..."
  curl -LO "${url}"

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

  install_kustomize "${os}" "${arch}"

  echo "Kustomize installation completed."
}

# Run the main function
main