package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"golang.org/x/time/rate"
)

// Config 应用配置
type Config struct {
	Rate            rate.Limit    // 每秒允许的请求数
	Burst           int           // 令牌桶的容量
	Port            string        // 服务端口
	CleanupInterval time.Duration // 清理过期IP的间隔
}

// RateLimiter IP限流器
type RateLimiter struct {
	mu    *sync.RWMutex
	ips   map[string]*ipLimiter
	rate  rate.Limit
	burst int
}

type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter 创建一个新的限流器
func NewRateLimiter(rate rate.Limit, burst int) *RateLimiter {
	return &RateLimiter{
		ips:   make(map[string]*ipLimiter),
		mu:    &sync.RWMutex{},
		rate:  rate,
		burst: burst,
	}
}

// AddIP 添加新的IP限流器
func (r *RateLimiter) AddIP(ip string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()

	limiter := &ipLimiter{
		limiter:  rate.NewLimiter(r.rate, r.burst),
		lastSeen: time.Now(),
	}
	r.ips[ip] = limiter
	return limiter.limiter
}

// GetLimiter 获取IP对应的限流器，如果不存在则创建
func (r *RateLimiter) GetLimiter(ip string) *rate.Limiter {
	r.mu.RLock()
	limiter, exists := r.ips[ip]
	r.mu.RUnlock()

	if !exists {
		return r.AddIP(ip)
	}

	r.mu.Lock()
	limiter.lastSeen = time.Now()
	r.mu.Unlock()

	return limiter.limiter
}

// CleanupStaleIPs 清理过期的IP限流器
func (r *RateLimiter) CleanupStaleIPs(maxAge time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for ip, limiter := range r.ips {
		if time.Since(limiter.lastSeen) > maxAge {
			delete(r.ips, ip)
		}
	}
}

var (
	// 默认配置
	config = Config{
		Rate:            1, // 每秒1个请求
		Burst:           5, // 最多允许5个并发请求
		Port:            ":8080",
		CleanupInterval: 5 * time.Minute,
	}
	limiter = NewRateLimiter(config.Rate, config.Burst)
)

// ImageResult 图片爬取结果
type ImageResult struct {
	URL       string `json:"url"`
	AltText   string `json:"alt_text,omitempty"`
	FileType  string `json:"file_type"`
	IsBase64  bool   `json:"is_base64,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
}

// ScrapeResponse 爬虫响应
type ScrapeResponse struct {
	Status  string        `json:"status"`
	Message string        `json:"message,omitempty"`
	Images  []ImageResult `json:"images"`
}

// 添加新的结构体来跟踪已访问的URL
type Crawler struct {
	visitedURLs map[string]bool
	maxDepth    int
	domain      string // 添加当前域名字段
	mu          sync.Mutex
}

// 修改 scrapeImages 函数，添加域名解析
func scrapeImages(targetURL string, depth int) ([]ImageResult, error) {
	// 解析目标URL获取域名
	parsedURL, err := neturl.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %v", err)
	}

	crawler := &Crawler{
		visitedURLs: make(map[string]bool),
		maxDepth:    1,
		domain:      parsedURL.Host, // 设置目标域名
	}

	var allImages []ImageResult
	images, err := crawler.scrape(targetURL, depth)
	if err != nil {
		return nil, err
	}
	allImages = append(allImages, images...)

	return allImages, nil
}

func (c *Crawler) scrape(url string, currentDepth int) ([]ImageResult, error) {
	if currentDepth > c.maxDepth {
		return nil, nil
	}

	// 检查URL是否已经访问过
	c.mu.Lock()
	if c.visitedURLs[url] {
		c.mu.Unlock()
		return nil, nil
	}
	c.visitedURLs[url] = true
	c.mu.Unlock()

	collector := colly.NewCollector(
		colly.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"),
		colly.MaxDepth(c.maxDepth),
		// 添加域名过滤器
		colly.AllowedDomains(c.domain),
	)

	var images []ImageResult
	var mu sync.Mutex // 用于保护images切片

	// 设置限速
	collector.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		RandomDelay: 1 * time.Second,
		Parallelism: 2,
	})

	// 处理图片
	collector.OnHTML("img[src]", func(e *colly.HTMLElement) {
		src := e.Attr("src")
		isBase64 := strings.HasPrefix(src, "data:image")

		if !isBase64 {
			if !strings.HasPrefix(src, "http") {
				src = e.Request.AbsoluteURL(src)
			}
		}

		if src == "" {
			return
		}

		var fileType string
		if isBase64 {
			if strings.Contains(src, "data:image/jpeg") {
				fileType = ".jpg"
			} else if strings.Contains(src, "data:image/png") {
				fileType = ".png"
			} else if strings.Contains(src, "data:image/gif") {
				fileType = ".gif"
			} else if strings.Contains(src, "data:image/webp") {
				fileType = ".webp"
			} else {
				fileType = ".jpg"
			}
		} else {
			fileType = filepath.Ext(src)
			if fileType == "" {
				fileType = ".jpg"
			}
		}

		image := ImageResult{
			URL:      src,
			AltText:  e.Attr("alt"),
			FileType: fileType,
			IsBase64: isBase64,
		}

		// 立即保存图片
		localPath, err := saveImage(image)
		if err != nil {
			fmt.Printf("Error saving image %s: %v\n", image.URL, err)
			return
		}
		image.LocalPath = localPath

		mu.Lock()
		images = append(images, image)
		mu.Unlock()
	})

	// 处理链接，进行递归爬取
	if currentDepth < c.maxDepth {
		collector.OnHTML("a[href]", func(e *colly.HTMLElement) {
			nextURL := e.Request.AbsoluteURL(e.Attr("href"))
			if nextURL != "" && !c.visitedURLs[nextURL] {
				// 由于已经设置了 AllowedDomains，这里不需要额外的域名检查
				nextImages, err := c.scrape(nextURL, currentDepth+1)
				if err == nil {
					mu.Lock()
					images = append(images, nextImages...)
					mu.Unlock()
				}
			}
		})
	}

	// 错误处理
	collector.OnError(func(r *colly.Response, err error) {
		fmt.Printf("Error scraping '%s': %v\n", r.Request.URL, err)
	})

	// 访问目标URL
	err := collector.Visit(url)
	if err != nil {
		return nil, fmt.Errorf("failed to visit URL: %v", err)
	}

	return images, nil
}

// 添加一个新的函数来获取域名和文件名
func getImagePathInfo(imageURL string) (domain string, filename string, err error) {
	fmt.Println("current crawl imageURL", imageURL)
	if strings.HasPrefix(imageURL, "data:image") {
		// 对于base64图片，使用"base64"作为域名
		return "base64", "", nil
	}

	// 解析URL获取域名
	parsedURL, err := neturl.Parse(imageURL)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse URL: %v", err)
	}

	domain = parsedURL.Host
	if domain == "" {
		domain = "unknown"
	}

	// 获取原始文件名
	urlPath := parsedURL.Path
	filename = filepath.Base(urlPath)

	// 如果没有文件名，返回空字符串
	if filename == "" || filename == "." || filename == "/" {
		filename = ""
	}

	return domain, filename, nil
}

// 修改 saveImage 函数
func saveImage(image ImageResult) (string, error) {
	// 获取域名和文件名
	domain, originalFilename, err := getImagePathInfo(image.URL)
	if err != nil {
		return "", fmt.Errorf("failed to get image path info: %v", err)
	}

	// 创建域名目录
	domainDir := filepath.Join("images", domain)
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create domain directory: %v", err)
	}

	// 生成文件名
	var filename string
	if originalFilename != "" {
		// 如果有原始文件名，使用时间戳_原始文件名的格式
		ext := filepath.Ext(originalFilename)
		if ext == "" {
			ext = image.FileType
		}
		basename := strings.TrimSuffix(originalFilename, ext)
		filename = fmt.Sprintf("%s_%d%s", basename, time.Now().UnixNano(), ext)
	} else {
		// 如果没有原始文件名，使用之前的时间戳格式
		filename = fmt.Sprintf("image_%d%s", time.Now().UnixNano(), image.FileType)
	}

	// 构建完整的文件路径
	filepath := filepath.Join(domainDir, filename)

	if image.IsBase64 {
		return saveBase64Image(image.URL, filepath)
	}
	return saveURLImage(image.URL, filepath)
}

// saveBase64Image 保存base64编码的图片
func saveBase64Image(base64Str string, filepath string) (string, error) {
	// 从base64字符串中提取实际的图片数据
	i := strings.Index(base64Str, ",")
	if i < 0 {
		return "", fmt.Errorf("invalid base64 string")
	}
	rawBase64 := base64Str[i+1:]

	// 解码base64数据
	decoded, err := base64.StdEncoding.DecodeString(rawBase64)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %v", err)
	}

	// 写入文件
	err = os.WriteFile(filepath, decoded, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to save image: %v", err)
	}

	return filepath, nil
}

// saveURLImage 下载并保存URL图片
func saveURLImage(url string, filepath string) (string, error) {
	// 发送GET请求获取图片
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to download image: %v", err)
	}
	defer resp.Body.Close()

	// 创建目标文件
	file, err := os.Create(filepath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	// 将响应内容复制到文件
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to save image: %v", err)
	}

	return filepath, nil
}

func main() {
	// 启动清理过期IP的goroutine
	go func() {
		ticker := time.NewTicker(config.CleanupInterval)
		for range ticker.C {
			limiter.CleanupStaleIPs(config.CleanupInterval)
		}
	}()

	// 设置路由
	mux := http.NewServeMux()
	mux.HandleFunc("/", rateLimitMiddleware(handleRequest))
	mux.HandleFunc("/scrape", rateLimitMiddleware(handleScrape))

	server := &http.Server{
		Addr:    config.Port,
		Handler: mux,
	}

	fmt.Printf("Server starting on port %s\n", config.Port)
	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}

// rateLimitMiddleware 限流中间件
func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		limiter := limiter.GetLimiter(ip)

		if !limiter.Allow() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// handleRequest 处理请求的处理器
func handleRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success", "message": "Hello, World!"}`))
}

// handleScrape 处理爬虫请求
func handleScrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var requestBody struct {
		URL string `json:"url"`
	}

	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if requestBody.URL == "" {
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}

	// 从深度1开始爬取
	images, err := scrapeImages(requestBody.URL, 1)
	if err != nil {
		response := ScrapeResponse{
			Status:  "error",
			Message: err.Error(),
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(response)
		return
	}

	// 保存图片到本地
	for i := range images {
		localPath, err := saveImage(images[i])
		if err != nil {
			fmt.Printf("Error saving image %s: %v\n", images[i].URL, err)
			continue
		}
		images[i].LocalPath = localPath
	}

	// 返回结果
	response := ScrapeResponse{
		Status: "success",
		Images: images,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}
