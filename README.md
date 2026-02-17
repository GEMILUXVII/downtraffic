# DownTraffic

Linux 下载流量消耗工具。通过并发下载公共文件并丢弃数据来消耗下载带宽，**磁盘零占用**。

## 特性

- 🚀 Go 编写，单个静态二进制文件，无运行时依赖
- 💾 数据直接丢弃到 `io.Discard`，磁盘零占用
- ⚡ goroutine 并发下载，充分利用带宽
- ⚖️ **对等模式**：自动读取网卡上下行数据，下载至对等后停止
- 📊 实时速率和累计流量统计
- ⏱️ 支持运行时长和流量上限限制
- 🔄 自动轮转多个下载源
- 🛑 Ctrl+C 优雅退出
- 🐧 systemd 服务支持，开机自启

## 快速开始

### 编译

```bash
# 在本机编译
go build -o downtraffic .

# 交叉编译 Linux amd64（Windows/macOS 上执行）
GOOS=linux GOARCH=amd64 go build -o downtraffic .

# 交叉编译 Linux arm64
GOOS=linux GOARCH=arm64 go build -o downtraffic .
```

> Windows PowerShell 交叉编译：
> ```powershell
> $env:GOOS="linux"; $env:GOARCH="amd64"; go build -o downtraffic .
> ```

### 运行

```bash
# 默认 4 线程，无限运行
./downtraffic

# 8 线程，运行 2 小时
./downtraffic -t 8 -d 2h

# 4 线程，下载 100GB 后自动停止
./downtraffic -t 4 -l 100G

# 使用自定义 URL 列表
./downtraffic -t 4 -f /path/to/urls.txt

# 6 线程，运行 1 天，上限 500GB
./downtraffic -t 6 -d 1d -l 500G
```

### ⚖️ 对等模式（自动平衡上下行）

```bash
# 自动检测网卡，下载至上下行对等后停止
./downtraffic -b

# 指定网卡
./downtraffic -b -i eth0

# 如果哪吒探针等监控显示已有上下行差距（如上行比下行多 1300G）
# 用 -offset 补偿这部分差距
./downtraffic -b -offset 1300G

# 完整示例：8 线程，指定网卡，补偿 1300G 差距
./downtraffic -b -t 8 -i ens3 -offset 1300G
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-t` | `4` | 并发下载线程数 |
| `-d` | `0` | 运行时长（`30s`, `5m`, `2h`, `1d`），0=无限 |
| `-l` | `0` | 总下载量上限（`100M`, `10G`, `1T`），0=无限 |
| `-f` | 内置列表 | URL 列表文件路径 |
| `-b` | `false` | 对等模式：自动计算上下行差距，下载至对等后停止 |
| `-i` | 自动检测 | 网卡名称（如 `eth0`, `ens3`） |
| `-offset` | `0` | 对等模式额外偏移量（监控中已有的差距，如 `1300G`） |
| `-v` | - | 显示版本号 |

## systemd 部署

### 一键安装

```bash
# 上传文件到服务器后执行
chmod +x install.sh
sudo ./install.sh install
```

### 手动管理

```bash
# 启动/停止/重启
sudo systemctl start downtraffic
sudo systemctl stop downtraffic
sudo systemctl restart downtraffic

# 查看状态
sudo systemctl status downtraffic

# 查看实时日志
sudo journalctl -u downtraffic -f

# 卸载
sudo ./install.sh uninstall
```

### 自定义参数

编辑 `/etc/systemd/system/downtraffic.service` 中的 `ExecStart` 行：

```ini
# 示例：8 线程，每天上限 1TB
ExecStart=/opt/downtraffic/downtraffic -t 8 -l 1T -f /opt/downtraffic/urls.txt
```

修改后重新加载：

```bash
sudo systemctl daemon-reload
sudo systemctl restart downtraffic
```

## URL 列表格式

`urls.txt` 每行一个 URL，`#` 开头为注释：

```
# Speed Test 服务器
https://speed.hetzner.de/1GB.bin
https://speed.hetzner.de/10GB.bin

# Linux ISO
https://releases.ubuntu.com/24.04/ubuntu-24.04.1-desktop-amd64.iso
```

## 磁盘占用

| 文件 | 大小 |
|------|------|
| `downtraffic` 二进制 | ~6 MB |
| `urls.txt` | < 1 KB |
| **总计** | **< 10 MB** |

下载的数据**不会**写入磁盘，全部通过 `io.Discard` 直接丢弃。

## License

MIT
