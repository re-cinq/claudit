#!/usr/bin/env bash
#
# shiftlog installation script
# Usage: curl -fsSL https://raw.githubusercontent.com/re-cinq/shift-log/main/scripts/install.sh | bash
#
# This script must be EXECUTED, never SOURCED
# WRONG: source install.sh (will exit your shell on errors)
# CORRECT: bash install.sh
# CORRECT: curl -fsSL ... | bash
#

set -e

GITHUB_REPO="re-cinq/shift-log"
BINARY_NAME="shiftlog"
GO_MODULE="github.com/re-cinq/shift-log"
MIN_GO_MAJOR=1
MIN_GO_MINOR=24

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Track where we installed so we can warn about duplicates
LAST_INSTALL_PATH=""

log_info() {
    echo -e "${BLUE}==>${NC} $1"
}

log_success() {
    echo -e "${GREEN}==>${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}==>${NC} $1"
}

log_error() {
    echo -e "${RED}Error:${NC} $1" >&2
}

# Check if a release has a specific asset
release_has_asset() {
    local release_json=$1
    local asset_name=$2

    if echo "$release_json" | grep -Fq "\"name\": \"$asset_name\""; then
        return 0
    fi

    return 1
}

# Re-sign binary for macOS to avoid slow Gatekeeper checks
resign_for_macos() {
    local binary_path=$1

    if [[ "$(uname -s)" != "Darwin" ]]; then
        return 0
    fi

    if ! command -v codesign &> /dev/null; then
        log_warning "codesign not found, skipping re-signing"
        return 0
    fi

    log_info "Re-signing binary for macOS..."
    codesign --remove-signature "$binary_path" 2>/dev/null || true
    if codesign --force --sign - "$binary_path"; then
        log_success "Binary re-signed for this machine"
    else
        log_warning "Failed to re-sign binary (non-fatal)"
    fi
}

# Detect OS and architecture
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Darwin)
            os="darwin"
            ;;
        Linux)
            os="linux"
            ;;
        *)
            log_error "Unsupported operating system: $(uname -s)"
            exit 1
            ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)
            arch="amd64"
            ;;
        aarch64|arm64)
            arch="arm64"
            ;;
        *)
            log_error "Unsupported architecture: $(uname -m)"
            exit 1
            ;;
    esac

    echo "${os}_${arch}"
}

# Determine install directory
get_install_dir() {
    if [[ -w /usr/local/bin ]]; then
        echo "/usr/local/bin"
    else
        local dir="$HOME/.local/bin"
        mkdir -p "$dir"
        echo "$dir"
    fi
}

# Warn if install dir is not in PATH
warn_if_not_in_path() {
    local install_dir=$1

    if [[ ":$PATH:" != *":$install_dir:"* ]]; then
        log_warning "$install_dir is not in your PATH"
        echo ""
        echo "Add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
        echo "  export PATH=\"\$PATH:$install_dir\""
        echo ""
    fi
}

# Download and install from GitHub releases
install_from_release() {
    log_info "Installing $BINARY_NAME from GitHub releases..."

    local platform=$1
    local tmp_dir
    tmp_dir=$(mktemp -d)

    # Get latest release info
    log_info "Fetching latest release..."
    local latest_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    local release_json

    if command -v curl &> /dev/null; then
        release_json=$(curl -fsSL "$latest_url")
    elif command -v wget &> /dev/null; then
        release_json=$(wget -qO- "$latest_url")
    else
        log_error "Neither curl nor wget found. Please install one of them."
        return 1
    fi

    local version
    version=$(echo "$release_json" | grep '"tag_name"' | sed -E 's/.*"tag_name": "([^"]+)".*/\1/')

    if [ -z "$version" ]; then
        log_error "Failed to fetch latest version"
        rm -rf "$tmp_dir"
        return 1
    fi

    log_info "Latest version: $version"

    # Build archive name (matches goreleaser naming: shiftlog_{version}_{os}_{arch}.tar.gz)
    local archive_name="${BINARY_NAME}_${version#v}_${platform}.tar.gz"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${version}/${archive_name}"

    if ! release_has_asset "$release_json" "$archive_name"; then
        log_warning "No prebuilt archive available for platform ${platform}. Falling back to source installation methods."
        rm -rf "$tmp_dir"
        return 1
    fi

    log_info "Downloading $archive_name..."

    cd "$tmp_dir"
    if command -v curl &> /dev/null; then
        if ! curl -fsSL -o "$archive_name" "$download_url"; then
            log_error "Download failed"
            cd - > /dev/null || cd "$HOME"
            rm -rf "$tmp_dir"
            return 1
        fi
    elif command -v wget &> /dev/null; then
        if ! wget -q -O "$archive_name" "$download_url"; then
            log_error "Download failed"
            cd - > /dev/null || cd "$HOME"
            rm -rf "$tmp_dir"
            return 1
        fi
    fi

    # Extract archive
    log_info "Extracting archive..."
    if ! tar -xzf "$archive_name"; then
        log_error "Failed to extract archive"
        cd - > /dev/null || cd "$HOME"
        rm -rf "$tmp_dir"
        return 1
    fi

    # Install binary
    local install_dir
    install_dir=$(get_install_dir)

    log_info "Installing to $install_dir..."
    if [[ -w "$install_dir" ]]; then
        mv "$BINARY_NAME" "$install_dir/"
    else
        sudo mv "$BINARY_NAME" "$install_dir/"
    fi

    resign_for_macos "$install_dir/$BINARY_NAME"

    LAST_INSTALL_PATH="$install_dir/$BINARY_NAME"
    log_success "$BINARY_NAME installed to $install_dir/$BINARY_NAME"

    warn_if_not_in_path "$install_dir"

    cd - > /dev/null || cd "$HOME"
    rm -rf "$tmp_dir"
    return 0
}

# Check if Go is installed and meets minimum version
check_go() {
    if ! command -v go &> /dev/null; then
        return 1
    fi

    local go_version
    go_version=$(go version | awk '{print $3}' | sed 's/go//')
    log_info "Go detected: $(go version)"

    local major minor
    major=$(echo "$go_version" | cut -d. -f1)
    minor=$(echo "$go_version" | cut -d. -f2)

    if [ "$major" -eq "$MIN_GO_MAJOR" ] && [ "$minor" -lt "$MIN_GO_MINOR" ]; then
        log_error "Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR} or later is required (found: $go_version)"
        echo ""
        echo "Please upgrade Go:"
        echo "  - Download from https://go.dev/dl/"
        echo "  - Or use your package manager to update"
        echo ""
        return 1
    fi

    return 0
}

# Install using go install (fallback)
install_with_go() {
    log_info "Installing $BINARY_NAME using 'go install'..."

    if go install "${GO_MODULE}@latest"; then
        log_success "$BINARY_NAME installed successfully via go install"

        local bin_dir
        bin_dir=$(go env GOBIN 2>/dev/null || true)
        if [ -z "$bin_dir" ]; then
            bin_dir="$(go env GOPATH)/bin"
        fi

        LAST_INSTALL_PATH="$bin_dir/$BINARY_NAME"

        resign_for_macos "$bin_dir/$BINARY_NAME"

        warn_if_not_in_path "$bin_dir"
        return 0
    else
        log_error "go install failed"
        return 1
    fi
}

# Build from source (last resort)
build_from_source() {
    log_info "Building $BINARY_NAME from source..."

    local tmp_dir
    tmp_dir=$(mktemp -d)

    cd "$tmp_dir"
    log_info "Cloning repository..."

    if ! git clone --depth 1 "https://github.com/${GITHUB_REPO}.git"; then
        log_error "Failed to clone repository"
        cd - > /dev/null || cd "$HOME"
        rm -rf "$tmp_dir"
        return 1
    fi

    cd "$BINARY_NAME"
    log_info "Building binary..."

    if ! CGO_ENABLED=0 go build -o "$BINARY_NAME" .; then
        log_error "Build failed"
        cd - > /dev/null || cd "$HOME"
        rm -rf "$tmp_dir"
        return 1
    fi

    local install_dir
    install_dir=$(get_install_dir)

    log_info "Installing to $install_dir..."
    if [[ -w "$install_dir" ]]; then
        mv "$BINARY_NAME" "$install_dir/"
    else
        sudo mv "$BINARY_NAME" "$install_dir/"
    fi

    resign_for_macos "$install_dir/$BINARY_NAME"

    LAST_INSTALL_PATH="$install_dir/$BINARY_NAME"
    log_success "$BINARY_NAME installed to $install_dir/$BINARY_NAME"

    warn_if_not_in_path "$install_dir"

    cd - > /dev/null || cd "$HOME"
    rm -rf "$tmp_dir"
    return 0
}

# Returns all paths to the binary found in PATH (deduplicated)
get_binary_paths_in_path() {
    local IFS=':'
    local -a entries
    read -ra entries <<< "$PATH"
    local -a found
    local p
    for p in "${entries[@]}"; do
        [ -z "$p" ] && continue
        if [ -x "$p/$BINARY_NAME" ]; then
            local resolved
            if command -v readlink >/dev/null 2>&1; then
                resolved=$(readlink -f "$p/$BINARY_NAME" 2>/dev/null || printf '%s' "$p/$BINARY_NAME")
            else
                resolved="$p/$BINARY_NAME"
            fi
            local skip=0
            for existing in "${found[@]:-}"; do
                if [ "$existing" = "$resolved" ]; then skip=1; break; fi
            done
            if [ $skip -eq 0 ]; then
                found+=("$resolved")
            fi
        fi
    done
    for item in "${found[@]:-}"; do
        printf '%s\n' "$item"
    done
}

# Warn if multiple installations exist in PATH
warn_if_multiple_installations() {
    local paths=()
    while IFS= read -r line; do
        paths+=("$line")
    done < <(get_binary_paths_in_path)

    if [ "${#paths[@]}" -le 1 ]; then
        return 0
    fi

    log_warning "Multiple '$BINARY_NAME' executables found on your PATH. An older copy may be executed instead of the one we installed."
    echo "Found the following '$BINARY_NAME' executables (entries earlier in PATH take precedence):"
    local i=1
    for p in "${paths[@]}"; do
        local ver=""
        if [ -x "$p" ]; then
            ver=$("$p" --version 2>/dev/null || true)
        fi
        if [ -z "$ver" ]; then ver="<unknown version>"; fi
        echo "  $i. $p  -> $ver"
        i=$((i+1))
    done

    if [ -n "$LAST_INSTALL_PATH" ]; then
        echo ""
        echo "We installed to: $LAST_INSTALL_PATH"
        local first="${paths[0]}"
        if [ "$first" != "$LAST_INSTALL_PATH" ]; then
            log_warning "The '$BINARY_NAME' that appears first in your PATH is different from the one we installed."
            echo "  - Remove or rename the older $first, or"
            echo "  - Reorder your PATH so $(dirname "$LAST_INSTALL_PATH") appears before $(dirname "$first")"
            echo "After updating PATH, restart your shell and run '$BINARY_NAME version' to confirm."
        fi
    fi
}

# Verify installation succeeded
verify_installation() {
    warn_if_multiple_installations || true

    if command -v "$BINARY_NAME" &> /dev/null; then
        log_success "$BINARY_NAME is installed and ready!"
        echo ""
        "$BINARY_NAME" --version 2>/dev/null || echo "$BINARY_NAME (development build)"
        echo ""
        echo "Get started:"
        echo "  cd your-project"
        echo "  $BINARY_NAME init"
        echo ""
        return 0
    else
        log_error "$BINARY_NAME was installed but is not in PATH"
        return 1
    fi
}

# Main installation flow
main() {
    echo ""
    echo "shiftlog installer"
    echo ""

    log_info "Detecting platform..."
    local platform
    platform=$(detect_platform)
    log_info "Platform: $platform"

    # Try downloading from GitHub releases first
    if install_from_release "$platform"; then
        verify_installation
        exit 0
    fi

    log_warning "Failed to install from releases, trying alternative methods..."

    # Try go install as fallback
    if check_go; then
        if install_with_go; then
            verify_installation
            exit 0
        fi
    fi

    # Try building from source as last resort
    log_warning "Falling back to building from source..."

    if ! check_go; then
        log_warning "Go is not installed"
        echo ""
        echo "$BINARY_NAME requires Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR} or later to build from source. You can:"
        echo "  1. Install Go from https://go.dev/dl/"
        echo "  2. Use your package manager:"
        echo "     - macOS: brew install go"
        echo "     - Ubuntu/Debian: sudo apt install golang"
        echo "     - Other Linux: Check your distro's package manager"
        echo ""
        echo "After installing Go, run this script again."
        exit 1
    fi

    if build_from_source; then
        verify_installation
        exit 0
    fi

    # All methods failed
    log_error "Installation failed"
    echo ""
    echo "Manual installation:"
    echo "  1. Download from https://github.com/${GITHUB_REPO}/releases/latest"
    echo "  2. Extract and move '$BINARY_NAME' to your PATH"
    echo ""
    echo "Or install from source:"
    echo "  1. Install Go from https://go.dev/dl/"
    echo "  2. Run: go install ${GO_MODULE}@latest"
    echo ""
    exit 1
}

main "$@"
