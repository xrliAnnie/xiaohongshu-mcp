package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
)

func main() {
	var (
		guardedConfig string
		schemaDigest  bool
		headless      bool
		binPath       string // 浏览器二进制文件路径
		port          string
	)
	flag.StringVar(&guardedConfig, "guarded-config", "", "immutable guarded provider configuration")
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

	if guardedConfig != "" {
		logrus.SetOutput(io.Discard)
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		if runGuardedProvider(ctx, guardedConfig) != nil {
			fmt.Fprintln(os.Stderr, "guarded_provider_unavailable")
			os.Exit(1)
		}
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
