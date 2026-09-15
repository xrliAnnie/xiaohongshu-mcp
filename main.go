package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
)

func main() {
	var (
		schemaDigest bool
		headless     bool
		binPath      string // 浏览器二进制文件路径
		port         string
	)
	flag.BoolVar(&schemaDigest, "guarded-schema-digest", false, "print the registered tool schema fingerprint and exit")
	flag.BoolVar(&headless, "headless", true, "是否无头模式")
	flag.StringVar(&binPath, "bin", "", "浏览器二进制文件路径")
	flag.StringVar(&port, "port", ":18060", "端口")
	flag.Parse()
	if schemaDigest {
		digest, err := measuredGuardedSchema(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, "guarded_schema_unavailable")
			os.Exit(1)
		}
		fmt.Println(digest)
		return
	}

	if len(binPath) == 0 {
		binPath = os.Getenv("ROD_BROWSER_BIN")
	}

	configs.InitHeadless(headless)
	configs.SetBinPath(binPath)

	// 初始化服务
	xiaohongshuService := NewXiaohongshuService()

	// 创建并启动应用服务器
	appServer := NewAppServer(xiaohongshuService)
	if err := appServer.Start(port); err != nil {
		logrus.Fatalf("failed to run server: %v", err)
	}
}
