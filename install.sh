#!/bin/bash
# DownTraffic 安装/卸载脚本
set -e

INSTALL_DIR="/opt/downtraffic"
SERVICE_FILE="/etc/systemd/system/downtraffic.service"
BINARY_NAME="downtraffic"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "请以 root 权限运行此脚本 (sudo ./install.sh)"
        exit 1
    fi
}

install() {
    check_root
    log_info "开始安装 DownTraffic..."

    # 检查二进制文件
    if [ ! -f "./${BINARY_NAME}" ]; then
        log_error "未找到 ${BINARY_NAME} 二进制文件"
        log_info "请先编译: GOOS=linux GOARCH=amd64 go build -o ${BINARY_NAME} ."
        exit 1
    fi

    # 停止已有服务
    if systemctl is-active --quiet downtraffic 2>/dev/null; then
        log_info "停止现有服务..."
        systemctl stop downtraffic
    fi

    # 创建安装目录
    log_info "安装到 ${INSTALL_DIR}..."
    mkdir -p "${INSTALL_DIR}"

    # 复制文件
    cp -f "./${BINARY_NAME}" "${INSTALL_DIR}/"
    chmod +x "${INSTALL_DIR}/${BINARY_NAME}"

    if [ -f "./urls.txt" ]; then
        cp -f "./urls.txt" "${INSTALL_DIR}/"
    fi

    # 安装 systemd 服务
    log_info "安装 systemd 服务..."
    cp -f "./downtraffic.service" "${SERVICE_FILE}"
    systemctl daemon-reload
    systemctl enable downtraffic

    log_info "安装完成！"
    echo ""
    echo "使用方法："
    echo "  启动服务:  sudo systemctl start downtraffic"
    echo "  停止服务:  sudo systemctl stop downtraffic"
    echo "  查看状态:  sudo systemctl status downtraffic"
    echo "  查看日志:  sudo journalctl -u downtraffic -f"
    echo ""
    echo "直接运行:    ${INSTALL_DIR}/${BINARY_NAME} -t 4 -f ${INSTALL_DIR}/urls.txt"
    echo ""

    read -p "是否立即启动服务? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        systemctl start downtraffic
        log_info "服务已启动"
        systemctl status downtraffic --no-pager
    fi
}

uninstall() {
    check_root
    log_info "开始卸载 DownTraffic..."

    # 停止并禁用服务
    if systemctl is-active --quiet downtraffic 2>/dev/null; then
        log_info "停止服务..."
        systemctl stop downtraffic
    fi
    if systemctl is-enabled --quiet downtraffic 2>/dev/null; then
        systemctl disable downtraffic
    fi

    # 删除文件
    [ -f "${SERVICE_FILE}" ] && rm -f "${SERVICE_FILE}"
    [ -d "${INSTALL_DIR}" ] && rm -rf "${INSTALL_DIR}"
    systemctl daemon-reload

    log_info "卸载完成！"
}

status() {
    if systemctl is-active --quiet downtraffic 2>/dev/null; then
        log_info "服务运行中"
        systemctl status downtraffic --no-pager
    else
        log_warn "服务未运行"
    fi
}

# 主入口
case "${1:-}" in
    install|"")
        install
        ;;
    uninstall|remove)
        uninstall
        ;;
    status)
        status
        ;;
    *)
        echo "用法: $0 {install|uninstall|status}"
        exit 1
        ;;
esac
