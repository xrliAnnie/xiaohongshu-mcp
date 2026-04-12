package configs

import (
	"os"
	"strings"
)

var (
	useHeadless = true

	binPath = ""

	// 默认 xiaohongshu.com，Rednote 用户设 XHS_BASE_URL=https://www.rednote.com
	baseURL = ""
)

// InitBaseURL 初始化 base URL，优先环境变量，规范化去尾斜杠
func InitBaseURL() {
	if v := os.Getenv("XHS_BASE_URL"); v != "" {
		baseURL = strings.TrimRight(v, "/")
	} else {
		baseURL = "https://www.xiaohongshu.com"
	}
}

// BaseURL 返回当前 base URL（无尾斜杠）
func BaseURL() string {
	if baseURL == "" {
		InitBaseURL()
	}
	return baseURL
}

// IsRednote 判断当前是否为 Rednote 模式
func IsRednote() bool {
	return strings.Contains(BaseURL(), "rednote.com")
}

// CreatorURL 返回创作者平台 URL
func CreatorURL() string {
	if IsRednote() {
		return "https://creator.rednote.com"
	}
	return "https://creator.xiaohongshu.com"
}

func InitHeadless(h bool) {
	useHeadless = h
}

// IsHeadless 是否无头模式。
func IsHeadless() bool {
	return useHeadless
}

func SetBinPath(b string) {
	binPath = b
}

func GetBinPath() string {
	return binPath
}
