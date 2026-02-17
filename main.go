package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ============================================================================
// DownTraffic - Linux 下载流量消耗工具
// 通过并发下载公共文件并丢弃数据来消耗下载带宽，磁盘零占用。
// ============================================================================

var (
	version = "1.1.0"

	// 内置默认 URL 列表（当无外部文件时使用）
	// 优先使用 HTTP 避免 TLS 证书问题
	defaultURLs = []string{
		// Tele2 Speed Test (稳定可靠)
		"http://speedtest.tele2.net/100MB.zip",
		"http://speedtest.tele2.net/1GB.zip",
		"http://speedtest.tele2.net/10GB.zip",
		// OVH Speed Test
		"http://proof.ovh.net/files/100Mb.dat",
		"http://proof.ovh.net/files/1Gb.dat",
		"http://proof.ovh.net/files/10Gb.dat",
		// Hetzner Speed Test (US Ashburn)
		"http://ash-speed.hetzner.com/100MB.bin",
		"http://ash-speed.hetzner.com/1GB.bin",
		"http://ash-speed.hetzner.com/10GB.bin",
		// ThinkBroadband (UK)
		"http://ipv4.download.thinkbroadband.com/100MB.zip",
		"http://ipv4.download.thinkbroadband.com/1GB.zip",
		// Hetzner Speed Test (DE) - HTTP
		"http://speed.hetzner.de/100MB.bin",
		"http://speed.hetzner.de/1GB.bin",
		"http://speed.hetzner.de/10GB.bin",
	}
)

// countingReader 包装 io.Reader，通过 atomic 计数器统计读取的字节数
type countingReader struct {
	reader  io.Reader
	counter *int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(cr.counter, int64(n))
	}
	return n, err
}

// formatBytes 将字节数格式化为人类可读的字符串
func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/float64(TB))
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatSpeed 将每秒字节数格式化为速率字符串
func formatSpeed(bytesPerSec int64) string {
	return formatBytes(bytesPerSec) + "/s"
}

// formatDuration 将 time.Duration 格式化为 HH:MM:SS
func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// parseDuration 解析时长字符串（支持 30s, 5m, 2h, 1d 等格式）
func parseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil // 0 表示无限运行
	}
	// 支持 "d" 天数单位
	if strings.HasSuffix(s, "d") {
		s = strings.TrimSuffix(s, "d")
		var days float64
		if _, err := fmt.Sscanf(s, "%f", &days); err != nil {
			return 0, fmt.Errorf("无法解析时长: %s", s)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// parseSize 解析大小字符串（如 100G, 500M, 1T）
func parseSize(s string) (int64, error) {
	if s == "" || s == "0" {
		return 0, nil // 0 表示无限制
	}
	s = strings.ToUpper(strings.TrimSpace(s))
	multiplier := int64(1)
	switch {
	case strings.HasSuffix(s, "T"):
		multiplier = 1024 * 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "G"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		multiplier = 1024
		s = strings.TrimSuffix(s, "K")
	}
	var val float64
	if _, err := fmt.Sscanf(s, "%f", &val); err != nil {
		return 0, fmt.Errorf("无法解析大小: %s", s)
	}
	return int64(val * float64(multiplier)), nil
}

// ============================================================================
// 网卡流量读取（对等模式核心）
// ============================================================================

// netStats 存储网卡的收发字节数
type netStats struct {
	RxBytes int64 // 接收（下载）
	TxBytes int64 // 发送（上传）
}

// getNetStats 从 /proc/net/dev 读取指定网卡的流量统计
func getNetStats(iface string) (*netStats, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("无法读取 /proc/net/dev: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 格式: iface: rx_bytes rx_packets ... tx_bytes tx_packets ...
		if !strings.HasPrefix(line, iface+":") {
			continue
		}
		parts := strings.Fields(strings.TrimPrefix(line, iface+":"))
		if len(parts) < 10 {
			return nil, fmt.Errorf("解析 /proc/net/dev 失败: 字段不足")
		}
		rxBytes, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析 RX 字节数失败: %v", err)
		}
		txBytes, err := strconv.ParseInt(parts[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析 TX 字节数失败: %v", err)
		}
		return &netStats{RxBytes: rxBytes, TxBytes: txBytes}, nil
	}
	return nil, fmt.Errorf("未找到网卡 %s", iface)
}

// detectInterface 自动检测主要网卡名称
func detectInterface() string {
	// 常见网卡名，按优先级排序
	candidates := []string{"eth0", "ens3", "ens18", "ens192", "enp0s3", "enp1s0", "venet0"}

	// 也扫描 /sys/class/net/ 下的实际网卡
	matches, _ := filepath.Glob("/sys/class/net/*")
	for _, m := range matches {
		name := filepath.Base(m)
		if name == "lo" || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "virbr") {
			continue
		}
		// 检查是否已在候选列表中
		found := false
		for _, c := range candidates {
			if c == name {
				found = true
				break
			}
		}
		if !found {
			candidates = append(candidates, name)
		}
	}

	for _, iface := range candidates {
		if _, err := getNetStats(iface); err == nil {
			return iface
		}
	}
	return "eth0" // fallback
}

// loadURLs 从文件加载 URL 列表，失败时返回默认列表
func loadURLs(path string) []string {
	if path == "" {
		return defaultURLs
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("⚠ 无法读取 URL 文件 %s，使用内置列表: %v", path, err)
		return defaultURLs
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		log.Printf("⚠ URL 文件为空，使用内置列表")
		return defaultURLs
	}
	log.Printf("✓ 已从 %s 加载 %d 个 URL", path, len(urls))
	return urls
}

// worker 是下载工作协程，从 urlCh 获取 URL 进行下载
func worker(ctx context.Context, id int, urls []string, counter *int64, wg *sync.WaitGroup) {
	defer wg.Done()

	client := &http.Client{
		Timeout: 0, // 不设整体超时，通过 context 控制
		Transport: &http.Transport{
			MaxIdleConns:        10,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // 禁用压缩以获得更大的传输量
			MaxIdleConnsPerHost: 2,
		},
	}

	urlIndex := rand.Intn(len(urls)) // 随机起始位置，避免所有 worker 同时下载同一文件

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		url := urls[urlIndex%len(urls)]
		urlIndex++

		if err := download(ctx, client, url, id, counter); err != nil {
			if ctx.Err() != nil {
				return // context 已取消，正常退出
			}
			log.Printf("  [W%d] ✗ 下载失败: %s (错误: %v)", id, truncateURL(url), err)
			time.Sleep(2 * time.Second) // 失败后等待一下再重试
		}
	}
}

// download 执行单次下载
func download(ctx context.Context, client *http.Client, url string, workerID int, counter *int64) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	// 设置 User-Agent 模拟正常浏览器
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	log.Printf("  [W%d] ↓ 开始下载: %s", workerID, truncateURL(url))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 使用 countingReader 包装 body，统计下载字节数
	cr := &countingReader{reader: resp.Body, counter: counter}

	// io.Discard 直接丢弃所有数据，不写入磁盘
	_, err = io.Copy(io.Discard, cr)
	if err != nil && ctx.Err() != nil {
		return nil // context 取消导致的错误，视为正常
	}
	return err
}

// truncateURL 截断过长的 URL 以便日志显示
func truncateURL(url string) string {
	const maxLen = 60
	if len(url) <= maxLen {
		return url
	}
	return url[:maxLen-3] + "..."
}

// statsReporter 定期打印下载统计信息
func statsReporter(ctx context.Context, counter *int64, startTime time.Time, limitBytes int64, balanceMode bool, iface string, offsetBytes int64) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastBytes int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentBytes := atomic.LoadInt64(counter)
			speed := currentBytes - lastBytes
			lastBytes = currentBytes
			elapsed := time.Since(startTime)

			// 构建统计行
			line := fmt.Sprintf("\r⚡ 速率: %-12s | 累计: %-10s | 时长: %s",
				formatSpeed(speed),
				formatBytes(currentBytes),
				formatDuration(elapsed),
			)

			if balanceMode && iface != "" {
				// 对等模式：显示实时上下行差距
				if stats, err := getNetStats(iface); err == nil {
					gap := stats.TxBytes + offsetBytes - stats.RxBytes
					if gap > 0 {
						line += fmt.Sprintf(" | 剩余差距: %s", formatBytes(gap))
					} else {
						line += " | ✓ 已对等"
					}
				}
			} else if limitBytes > 0 {
				progress := float64(currentBytes) / float64(limitBytes) * 100
				line += fmt.Sprintf(" | 进度: %.1f%%", progress)
			}
			fmt.Print(line)
		}
	}
}

type config struct {
	workers     int
	durationStr string
	limitStr    string
	urlFile     string
	urlCount    int
	balanceMode bool
	iface       string
	offsetStr   string
	gapBytes    int64
}

func printBanner(cfg *config) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║         DownTraffic v" + version + "                      ║")
	fmt.Println("║         Linux 下载流量消耗工具                  ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()
	if cfg.balanceMode {
		fmt.Println("  模式:     ⚖️  对等模式 (自动平衡上下行)")
		fmt.Printf("  网卡:     %s\n", cfg.iface)
		if cfg.offsetStr != "" && cfg.offsetStr != "0" {
			fmt.Printf("  额外偏移: %s\n", cfg.offsetStr)
		}
		fmt.Printf("  当前差距: %s (需下载)\n", formatBytes(cfg.gapBytes))
	}
	fmt.Printf("  并发数:   %d 个 worker\n", cfg.workers)
	if !cfg.balanceMode {
		if cfg.durationStr != "" && cfg.durationStr != "0" {
			fmt.Printf("  运行时长: %s\n", cfg.durationStr)
		} else {
			fmt.Printf("  运行时长: 无限制 (Ctrl+C 停止)\n")
		}
		if cfg.limitStr != "" && cfg.limitStr != "0" {
			fmt.Printf("  流量上限: %s\n", cfg.limitStr)
		} else {
			fmt.Printf("  流量上限: 无限制\n")
		}
	}
	fmt.Printf("  下载源:   %d 个 URL\n", cfg.urlCount)
	if cfg.urlFile != "" {
		fmt.Printf("  URL 文件: %s\n", cfg.urlFile)
	} else {
		fmt.Printf("  URL 文件: 内置列表\n")
	}
	fmt.Println()
	fmt.Println("  ⏳ 开始下载... (Ctrl+C 优雅退出)")
	fmt.Println()
}

func main() {
	// 解析命令行参数
	workers := flag.Int("t", 4, "并发下载线程数")
	durationStr := flag.String("d", "0", "运行时长 (如 30s, 5m, 2h, 1d)，0=无限")
	limitStr := flag.String("l", "0", "总下载量上限 (如 100G, 500M, 1T)，0=无限")
	urlFile := flag.String("f", "", "URL 列表文件路径 (每行一个URL，#开头为注释)")
	balanceMode := flag.Bool("b", false, "对等模式: 自动计算上下行差距，下载至对等后停止")
	iface := flag.String("i", "", "网卡名称 (默认自动检测，如 eth0, ens3)")
	offsetStr := flag.String("offset", "0", "对等模式额外偏移量，即监控中已有的上下行差距 (如 1300G)")
	showVersion := flag.Bool("v", false, "显示版本号")
	flag.Parse()

	if *showVersion {
		fmt.Printf("DownTraffic v%s\n", version)
		os.Exit(0)
	}

	if *workers < 1 {
		log.Fatal("✗ 线程数必须 >= 1")
	}

	// 对等模式只在 Linux 上可用
	if *balanceMode && runtime.GOOS != "linux" {
		log.Fatal("✗ 对等模式 (-b) 仅支持 Linux 系统")
	}

	// 解析运行时长
	duration, err := parseDuration(*durationStr)
	if err != nil {
		log.Fatalf("✗ 无效的时长格式: %v", err)
	}

	// 解析流量上限
	limitBytes, err := parseSize(*limitStr)
	if err != nil {
		log.Fatalf("✗ 无效的大小格式: %v", err)
	}

	// 解析偏移量
	offsetBytes, err := parseSize(*offsetStr)
	if err != nil {
		log.Fatalf("✗ 无效的偏移量格式: %v", err)
	}

	// 对等模式：计算需要下载的量
	var gapBytes int64
	actualIface := *iface
	if *balanceMode {
		if actualIface == "" {
			actualIface = detectInterface()
		}
		stats, err := getNetStats(actualIface)
		if err != nil {
			log.Fatalf("✗ 读取网卡 %s 流量失败: %v", actualIface, err)
		}
		// 差距 = (上行 + 额外偏移) - 下行
		gapBytes = stats.TxBytes + offsetBytes - stats.RxBytes
		if gapBytes <= 0 {
			fmt.Println("\n✓ 上下行已经对等（下行 >= 上行），无需额外下载")
			fmt.Printf("  网卡:   %s\n", actualIface)
			fmt.Printf("  上行:   %s\n", formatBytes(stats.TxBytes+offsetBytes))
			fmt.Printf("  下行:   %s\n", formatBytes(stats.RxBytes))
			os.Exit(0)
		}
		// 在对等模式下，将 gap 设为下载上限
		limitBytes = gapBytes
		log.Printf("✓ 检测到网卡 %s | 上行: %s | 下行: %s | 差距: %s",
			actualIface,
			formatBytes(stats.TxBytes+offsetBytes),
			formatBytes(stats.RxBytes),
			formatBytes(gapBytes),
		)
	}

	// 加载 URL 列表
	urls := loadURLs(*urlFile)

	// 打印启动信息
	cfg := &config{
		workers:     *workers,
		durationStr: *durationStr,
		limitStr:    *limitStr,
		urlFile:     *urlFile,
		urlCount:    len(urls),
		balanceMode: *balanceMode,
		iface:       actualIface,
		offsetStr:   *offsetStr,
		gapBytes:    gapBytes,
	}
	printBanner(cfg)

	// 创建可取消的 context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 如果设置了运行时长，添加超时
	if duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	// 捕获系统信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 字节计数器
	var totalBytes int64
	startTime := time.Now()

	// 启动统计打印协程
	go statsReporter(ctx, &totalBytes, startTime, limitBytes, *balanceMode, actualIface, offsetBytes)

	// 流量上限检查协程（普通模式或对等模式都使用）
	if limitBytes > 0 {
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if *balanceMode {
						// 对等模式：实时读取网卡数据判断是否已对等
						if stats, err := getNetStats(actualIface); err == nil {
							gap := stats.TxBytes + offsetBytes - stats.RxBytes
							if gap <= 0 {
								fmt.Printf("\n\n✓ 上下行已对等！正在停止...\n")
								cancel()
								return
							}
						}
					} else {
						// 普通模式：按累计下载量判断
						if atomic.LoadInt64(&totalBytes) >= limitBytes {
							log.Printf("\n\n✓ 已达到流量上限 %s，正在停止...", formatBytes(limitBytes))
							cancel()
							return
						}
					}
				}
			}
		}()
	}

	// 启动 worker 协程
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go worker(ctx, i+1, urls, &totalBytes, &wg)
	}

	// 等待信号或完成
	select {
	case sig := <-sigCh:
		fmt.Printf("\n\n⚠ 收到信号 %v，正在优雅退出...\n", sig)
		cancel()
	case <-ctx.Done():
	}

	// 等待所有 worker 退出
	wg.Wait()

	// 打印最终统计
	elapsed := time.Since(startTime)
	finalBytes := atomic.LoadInt64(&totalBytes)
	avgSpeed := int64(0)
	if elapsed.Seconds() > 0 {
		avgSpeed = int64(float64(finalBytes) / elapsed.Seconds())
	}

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║                  下载统计                       ║")
	fmt.Println("╠══════════════════════════════════════════════════╣")
	fmt.Printf("║  总下载量:   %-36s║\n", formatBytes(finalBytes))
	fmt.Printf("║  运行时长:   %-36s║\n", formatDuration(elapsed))
	fmt.Printf("║  平均速率:   %-36s║\n", formatSpeed(avgSpeed))
	fmt.Printf("║  并发线程:   %-36s║\n", fmt.Sprintf("%d", *workers))
	if *balanceMode {
		if stats, err := getNetStats(actualIface); err == nil {
			gap := stats.TxBytes + offsetBytes - stats.RxBytes
			if gap <= 0 {
				fmt.Printf("║  对等状态:   %-36s║\n", "✓ 已对等")
			} else {
				fmt.Printf("║  剩余差距:   %-36s║\n", formatBytes(gap))
			}
		}
	}
	fmt.Println("╚══════════════════════════════════════════════════╝")
}
