package configs

import "os"

var (
	useHeadless = true

	binPath = ""

	// 默认 xiaohongshu.com，Rednote 用户设 XHS_BASE_URL=https://www.rednote.com
	baseURL = ""
)

// InitBaseURL 初始化 base URL，优先环境变量
func InitBaseURL() {
	if v := os.Getenv("XHS_BASE_URL"); v != "" {
		baseURL = v
	} else {
		baseURL = "https://www.xiaohongshu.com"
	}
}

// BaseURL 返回当前 base URL（如 https://www.xiaohongshu.com 或 https://www.rednote.com）
func BaseURL() string {
	if baseURL == "" {
		InitBaseURL()
	}
	return baseURL
}

// CreatorURL 返回创作者平台 URL
func CreatorURL() string {
	if baseURL == "" {
		InitBaseURL()
	}
	if baseURL == "https://www.rednote.com" {
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
