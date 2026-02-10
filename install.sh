#!/usr/bin/env bash
# ============================================================
# conduit-expose installer
# One-liner: curl -sL <repo-url>/install.sh | sudo bash
# ============================================================
#
# The { ... } block forces bash to read the ENTIRE script into
# memory before executing. Without this, `curl | bash` reads
# in chunks and interactive `read` commands break.
#
{

set -euo pipefail

# ============================================================
# Colors & Logging
# ============================================================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
NC='\033[0m' # No Color

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}  [OK]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED} [ERR]${NC} $*"; }

# ============================================================
# TTY handling for curl | bash compatibility
# ============================================================
# Only open /dev/tty for interactive modes (install, uninstall).
# Non-interactive modes like update-ctl skip this.
if [ "${1:-}" != "update-ctl" ]; then
    if ! exec 3</dev/tty 2>/dev/null; then
        log_error "Cannot open /dev/tty for interactive input."
        log_error "Please download and run the script directly instead:"
        log_error "  wget -O install.sh https://github.com/omid3098/conduit_expose/raw/main/install.sh && sudo bash install.sh"
        exit 1
    fi
fi

# prompt "Prompt text" VARIABLE [DEFAULT]
prompt() {
    local text="$1" var_name="$2" default="${3:-}"
    if [ -n "$default" ]; then
        printf "%b [%b%s%b]: " "$text" "${GREEN}" "$default" "${NC}" >/dev/tty
    else
        printf "%b: " "$text" >/dev/tty
    fi
    local reply
    read -r reply <&3
    reply="${reply:-$default}"
    eval "$var_name=\$reply"
}

# confirm "Prompt text" [DEFAULT_YES]
confirm() {
    local text="$1" default="${2:-Y}"
    local hint="[Y/n]"
    [ "$default" = "N" ] && hint="[y/N]"
    printf "%b %s: " "$text" "$hint" >/dev/tty
    local reply
    read -r reply <&3
    reply="${reply:-$default}"
    [[ "$reply" =~ ^[Yy]$ ]]
}

# ============================================================
# Constants
# ============================================================
REPO_URL="https://github.com/omid3098/conduit_expose"
CONFIG_DIR="/etc/conduit-expose"
CONFIG_FILE="${CONFIG_DIR}/config"
CTL_PATH="/usr/local/bin/conduit-expose-ctl"
CONTAINER_NAME="conduit-expose"
IMAGE_NAME="conduit-expose"
INTERNAL_PORT=8081

# ============================================================
# Helpers
# ============================================================
generate_secret() {
    head -c 16 /dev/urandom | xxd -p 2>/dev/null \
        || openssl rand -hex 16 2>/dev/null \
        || cat /proc/sys/kernel/random/uuid | tr -d '-'
}

random_port() {
    shuf -i 10000-65000 -n 1 2>/dev/null || echo $(( RANDOM % 55000 + 10000 ))
}

get_server_ip() {
    hostname -I 2>/dev/null | awk '{print $1}' || hostname
}

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi
}

check_docker() {
    if ! command -v docker &>/dev/null; then
        log_warn "Docker is not installed."
        echo ""
        if confirm "$(echo -e "${CYAN}Install Docker automatically?${NC}")" "Y"; then
            install_docker_engine
        else
            log_error "Docker is required. Please install Docker and re-run."
            exit 1
        fi
    fi

    if ! docker info &>/dev/null; then
        log_error "Docker daemon is not running. Start it with: systemctl start docker"
        exit 1
    fi
    log_success "Docker is installed and running"
}

install_docker_engine() {
    log_info "Installing Docker..."
    if command -v apt-get &>/dev/null; then
        apt-get update -qq
        apt-get install -y -qq curl ca-certificates
        curl -fsSL https://get.docker.com | sh
    elif command -v yum &>/dev/null; then
        yum install -y curl
        curl -fsSL https://get.docker.com | sh
    elif command -v apk &>/dev/null; then
        apk add --no-cache docker
        rc-update add docker default 2>/dev/null || true
        service docker start 2>/dev/null || true
    elif command -v pacman &>/dev/null; then
        pacman -Sy --noconfirm docker
        systemctl enable --now docker
    else
        log_error "Unsupported package manager. Install Docker manually."
        exit 1
    fi

    if command -v systemctl &>/dev/null; then
        systemctl enable docker 2>/dev/null || true
        systemctl start docker 2>/dev/null || true
    fi

    if ! docker info &>/dev/null; then
        log_error "Docker installation failed. Please install manually."
        exit 1
    fi
    log_success "Docker installed successfully"
}

# ============================================================
# Install Action
# ============================================================
do_install() {
    echo ""
    echo -e "${CYAN}${BOLD}=====================================${NC}"
    echo -e "${CYAN}${BOLD}   conduit-expose installer${NC}"
    echo -e "${CYAN}${BOLD}=====================================${NC}"
    echo ""

    check_root
    check_docker

    # Check for existing installation
    if docker ps -a --format '{{.Names}}' | grep -qw "$CONTAINER_NAME"; then
        log_warn "Existing conduit-expose container found."
        if ! confirm "$(echo -e "${YELLOW}Reinstall? This will replace the current installation${NC}")" "N"; then
            log_info "Cancelled."
            exit 0
        fi
        log_info "Removing existing container..."
        docker stop "$CONTAINER_NAME" 2>/dev/null || true
        docker rm "$CONTAINER_NAME" 2>/dev/null || true
    fi

    echo ""
    echo -e "${BOLD}Configuration${NC}"
    echo -e "${DIM}Press Enter to accept defaults shown in brackets.${NC}"
    echo ""

    # --- Port selection ---
    local default_port port
    default_port=$(random_port)
    prompt "$(echo -e "${CYAN}Expose port${NC}")" port "$default_port"

    # Validate port
    if ! [[ "$port" =~ ^[0-9]+$ ]] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
        log_error "Invalid port: $port"
        exit 1
    fi

    # Check if port is in use
    if ss -tlnp 2>/dev/null | grep -q ":${port} " || netstat -tlnp 2>/dev/null | grep -q ":${port} "; then
        log_warn "Port $port appears to be in use. Choose a different port."
        prompt "$(echo -e "${CYAN}Expose port${NC}")" port ""
    fi

    # --- Auth secret ---
    local default_secret secret
    default_secret=$(generate_secret)
    prompt "$(echo -e "${CYAN}Auth secret${NC}")" secret "$default_secret"

    if [ -z "$secret" ]; then
        log_error "Auth secret cannot be empty."
        exit 1
    fi

    # --- Confirmation ---
    echo ""
    echo -e "${BOLD}Summary:${NC}"
    echo -e "  Port:    ${GREEN}${port}${NC}"
    echo -e "  Secret:  ${GREEN}${secret}${NC}"
    echo -e "  Image:   ${DIM}${IMAGE_NAME} (built locally)${NC}"
    echo ""
    if ! confirm "$(echo -e "${CYAN}Proceed with these settings?${NC}")" "Y"; then
        log_info "Cancelled."
        exit 0
    fi

    echo ""

    # --- Download source & build ---
    log_info "Downloading source..."
    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf '$tmp_dir'" EXIT

    if command -v git &>/dev/null; then
        git clone --depth 1 "$REPO_URL" "$tmp_dir/src" 2>/dev/null
    else
        log_info "git not found, downloading tarball..."
        curl -sL "${REPO_URL}/archive/refs/heads/main.tar.gz" -o "$tmp_dir/src.tar.gz"
        mkdir -p "$tmp_dir/src"
        tar -xzf "$tmp_dir/src.tar.gz" -C "$tmp_dir/src" --strip-components=1
    fi
    log_success "Source downloaded"

    log_info "Building Docker image (this may take a minute)..."
    docker build -t "$IMAGE_NAME" "$tmp_dir/src" --quiet
    log_success "Docker image built"

    # --- Deploy container ---
    log_info "Starting container..."
    docker run -d \
        --name "$CONTAINER_NAME" \
        --restart unless-stopped \
        --log-opt max-size=10m \
        --log-opt max-file=3 \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -e "CONDUIT_AUTH_SECRET=${secret}" \
        -p "${port}:${INTERNAL_PORT}" \
        "$IMAGE_NAME" >/dev/null

    log_success "Container started"

    # --- Save config ---
    local server_ip
    server_ip=$(get_server_ip)
    local connection_uri="conduit://${secret}@${server_ip}:${port}"

    mkdir -p "$CONFIG_DIR"
    cat > "$CONFIG_FILE" <<CONF
PORT=${port}
AUTH_SECRET=${secret}
SERVER_IP=${server_ip}
CONNECTION_URI=${connection_uri}
CONTAINER_NAME=${CONTAINER_NAME}
IMAGE_NAME=${IMAGE_NAME}
INSTALLED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
CONF
    chmod 600 "$CONFIG_FILE"
    log_success "Config saved to $CONFIG_FILE"

    # --- Install management CLI ---
    install_ctl

    # --- Done ---
    echo ""
    echo -e "${GREEN}${BOLD}=====================================${NC}"
    echo -e "${GREEN}${BOLD}   Installation complete!${NC}"
    echo -e "${GREEN}${BOLD}=====================================${NC}"
    echo ""
    echo -e "  ${BOLD}Connection URI${NC} ${DIM}(paste into your monitoring dashboard):${NC}"
    echo ""
    echo -e "  ${GREEN}${connection_uri}${NC}"
    echo ""
    echo -e "  ${BOLD}Manage:${NC}  conduit-expose-ctl [status|restart|update|uninstall|show-config|uri]"
    echo ""
}

# ============================================================
# Management CLI
# ============================================================
install_ctl() {
    cat > "$CTL_PATH" <<'CTLSCRIPT'
#!/usr/bin/env bash
set -euo pipefail

CONFIG_FILE="/etc/conduit-expose/config"
CONTAINER_NAME="conduit-expose"
IMAGE_NAME="conduit-expose"
REPO_URL="https://github.com/omid3098/conduit_expose"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'
DIM='\033[2m'; NC='\033[0m'

log_info()    { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}  [OK]${NC} $*"; }
log_warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error()   { echo -e "${RED} [ERR]${NC} $*"; }

load_config() {
    if [ ! -f "$CONFIG_FILE" ]; then
        log_error "Config not found at $CONFIG_FILE. Is conduit-expose installed?"
        exit 1
    fi
    # shellcheck disable=SC1090
    source "$CONFIG_FILE"
}

cmd_status() {
    load_config
    echo ""
    echo -e "${BOLD}conduit-expose status${NC}"
    echo ""
    if docker ps --format '{{.Names}}' | grep -qw "$CONTAINER_NAME"; then
        local info
        info=$(docker inspect --format '{{.State.Status}} | Up since {{.State.StartedAt}}' "$CONTAINER_NAME" 2>/dev/null)
        echo -e "  State:     ${GREEN}running${NC} (${DIM}${info}${NC})"
    else
        echo -e "  State:     ${RED}stopped${NC}"
    fi
    echo -e "  Port:      ${PORT:-unknown}"
    echo -e "  Container: ${CONTAINER_NAME}"
    echo ""
}

cmd_restart() {
    load_config
    log_info "Restarting conduit-expose..."
    docker restart "$CONTAINER_NAME"
    log_success "Restarted"
}

cmd_update() {
    load_config
    log_info "Updating conduit-expose..."

    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap "rm -rf '$tmp_dir'" EXIT

    if command -v git &>/dev/null; then
        git clone --depth 1 "$REPO_URL" "$tmp_dir/src" 2>/dev/null
    else
        curl -sL "${REPO_URL}/archive/refs/heads/main.tar.gz" -o "$tmp_dir/src.tar.gz"
        mkdir -p "$tmp_dir/src"
        tar -xzf "$tmp_dir/src.tar.gz" -C "$tmp_dir/src" --strip-components=1
    fi
    log_success "Source downloaded"

    log_info "Rebuilding image..."
    docker build -t "$IMAGE_NAME" "$tmp_dir/src" --quiet
    log_success "Image rebuilt"

    # Self-update: overwrite this script with the latest version
    log_info "Updating management CLI..."
    bash "$tmp_dir/src/install.sh" update-ctl
    log_success "Management CLI updated"

    log_info "Recreating container (preserving config)..."
    docker stop "$CONTAINER_NAME" 2>/dev/null || true
    docker rm "$CONTAINER_NAME" 2>/dev/null || true

    docker run -d \
        --name "$CONTAINER_NAME" \
        --restart unless-stopped \
        --log-opt max-size=10m \
        --log-opt max-file=3 \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -e "CONDUIT_AUTH_SECRET=${AUTH_SECRET}" \
        -p "${PORT}:8081" \
        "$IMAGE_NAME" >/dev/null

    log_success "Updated and running on port ${PORT}"
}

cmd_uninstall() {
    load_config
    echo ""
    log_warn "This will remove conduit-expose completely."
    read -rp "$(echo -e "${YELLOW}Are you sure? [y/N]:${NC} ")" confirm
    if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
        log_info "Cancelled."
        exit 0
    fi

    log_info "Stopping and removing container..."
    docker stop "$CONTAINER_NAME" 2>/dev/null || true
    docker rm "$CONTAINER_NAME" 2>/dev/null || true

    log_info "Removing image..."
    docker rmi "$IMAGE_NAME" 2>/dev/null || true

    log_info "Removing config..."
    rm -rf /etc/conduit-expose

    log_info "Removing management CLI..."
    rm -f /usr/local/bin/conduit-expose-ctl

    log_success "conduit-expose uninstalled"
    echo ""
}

cmd_show_config() {
    load_config
    echo ""
    echo -e "${BOLD}conduit-expose config${NC}"
    echo ""
    echo -e "  ${BOLD}Connection URI:${NC}"
    echo -e "  ${GREEN}${CONNECTION_URI:-conduit://${AUTH_SECRET}@${SERVER_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}:${PORT}}${NC}"
    echo ""
    echo -e "  Port:       ${PORT}"
    echo -e "  Secret:     ${AUTH_SECRET}"
    echo -e "  Container:  ${CONTAINER_NAME}"
    echo -e "  Installed:  ${INSTALLED_AT:-unknown}"
    echo ""
}

cmd_uri() {
    load_config
    local uri="${CONNECTION_URI:-conduit://${AUTH_SECRET}@${SERVER_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}:${PORT}}"
    echo "$uri"
}

case "${1:-}" in
    status)      cmd_status ;;
    restart)     cmd_restart ;;
    update)      cmd_update ;;
    uninstall)   cmd_uninstall ;;
    show-config) cmd_show_config ;;
    uri)         cmd_uri ;;
    *)
        echo "Usage: conduit-expose-ctl {status|restart|update|uninstall|show-config|uri}"
        exit 1
        ;;
esac
CTLSCRIPT

    chmod +x "$CTL_PATH"
    log_success "Management CLI installed to $CTL_PATH"
}

# ============================================================
# Entrypoint
# ============================================================
case "${1:-}" in
    install|"")  do_install ;;
    uninstall)
        source "$CONFIG_FILE" 2>/dev/null || true
        check_root
        log_warn "This will remove conduit-expose completely."
        if confirm "$(echo -e "${YELLOW}Are you sure?${NC}")" "N"; then
            docker stop "$CONTAINER_NAME" 2>/dev/null || true
            docker rm "$CONTAINER_NAME" 2>/dev/null || true
            docker rmi "$IMAGE_NAME" 2>/dev/null || true
            rm -rf "$CONFIG_DIR"
            rm -f "$CTL_PATH"
            log_success "conduit-expose uninstalled"
        fi
        ;;
    update-ctl)
        # Silent mode: only regenerate the ctl script.
        # Called by `conduit-expose-ctl update` during self-update.
        install_ctl
        ;;
    *)
        echo "Usage: install.sh [install|uninstall]"
        exit 1
        ;;
esac

# Close tty fd (only if it was opened)
exec 3<&- 2>/dev/null || true

# exit prevents bash from trying to read more from the pipe after the block
exit 0
}
