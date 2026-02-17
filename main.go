package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
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
	version = "1.0.0"

	// 内置默认 URL 列表（当无外部文件时使用）
	defaultURLs = []string{
		// Hetzner Speed Test
		"https://speed.hetzner.de/100MB.bin",
		"https://speed.hetzner.de/1GB.bin",
		"https://speed.hetzner.de/10GB.bin",
		// OVH Speed Test
		"http://proof.ovh.net/files/100Mb.dat",
		"http://proof.ovh.net/files/1Gb.dat",
		// Scaleway Speed Test
		"https://multi.speedtest.net/10M",
		"https://multi.speedtest.net/100M",
		// Tele2 Speed Test
		"https://ash-speed.hetzner.com/100MB.bin",
		"https://ash-speed.hetzner.com/1GB.bin",
		// Linux ISOs
		"https://releases.ubuntu.com/24.04/ubuntu-24.04.1-desktop-amd64.iso",
		"https://cdimage.debian.org/debian-cd/current/amd64/iso-cd/debian-12.9.0-amd64-netinst.iso",
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
func statsReporter(ctx context.Context, counter *int64, startTime time.Time, limitBytes int64) {
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
			if limitBytes > 0 {
				progress := float64(currentBytes) / float64(limitBytes) * 100
				line += fmt.Sprintf(" | 进度: %.1f%%", progress)
			}
			fmt.Print(line)
		}
	}
}

func printBanner(workers int, duration string, limit string, urlFile string, urlCount int) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║         DownTraffic v" + version + "                      ║")
	fmt.Println("║         Linux 下载流量消耗工具                  ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  并发数:   %d 个 worker\n", workers)
	if duration != "" && duration != "0" {
		fmt.Printf("  运行时长: %s\n", duration)
	} else {
		fmt.Printf("  运行时长: 无限制 (Ctrl+C 停止)\n")
	}
	if limit != "" && limit != "0" {
		fmt.Printf("  流量上限: %s\n", limit)
	} else {
		fmt.Printf("  流量上限: 无限制\n")
	}
	fmt.Printf("  下载源:   %d 个 URL\n", urlCount)
	if urlFile != "" {
		fmt.Printf("  URL 文件: %s\n", urlFile)
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
	showVersion := flag.Bool("v", false, "显示版本号")
	flag.Parse()

	if *showVersion {
		fmt.Printf("DownTraffic v%s\n", version)
		os.Exit(0)
	}

	if *workers < 1 {
		log.Fatal("✗ 线程数必须 >= 1")
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

	// 加载 URL 列表
	urls := loadURLs(*urlFile)

	// 打印启动信息
	printBanner(*workers, *durationStr, *limitStr, *urlFile, len(urls))

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
	go statsReporter(ctx, &totalBytes, startTime, limitBytes)

	// 流量上限检查协程
	if limitBytes > 0 {
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if atomic.LoadInt64(&totalBytes) >= limitBytes {
						log.Printf("\n\n✓ 已达到流量上限 %s，正在停止...", formatBytes(limitBytes))
						cancel()
						return
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
	fmt.Println("╚══════════════════════════════════════════════════╝")
}
