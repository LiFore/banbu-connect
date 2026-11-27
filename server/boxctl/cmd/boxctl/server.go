package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"boxctl/internal/dist"
	"boxctl/internal/otel"
	"boxctl/internal/panel"
)

// runServer 运行 Banbu Connect 的管理服务：在网页上管理订阅、节点、代理组和线路，
// 发布给绑定的客户端，并托管客户端更新包。复用路由盒面板的登录和页面。
func runServer(ctx context.Context, logger *slog.Logger, args []string) error {
	if len(args) > 0 && args[0] == "export-seed" {
		return runExportSeed(ctx, args[1:])
	}
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	opts := panel.Options{Logger: logger, Server: true}
	fs.StringVar(&opts.Listen, "listen", "0.0.0.0:8443", "监听地址")
	fs.StringVar(&opts.DataDir, "data-dir", "/var/lib/banbu-connect", "数据目录（数据库、密钥、证书、更新包）")
	fs.StringVar(&opts.TLSCert, "tls-cert", "", "证书文件；为空时生成自签名证书（客户端绑定时要填证书指纹）")
	fs.StringVar(&opts.TLSKey, "tls-key", "", "私钥文件")
	fs.BoolVar(&opts.PlainHTTP, "plain-http", false, "只监听 HTTP，由前面的反向代理负责 HTTPS（此时 -listen 应为回环地址）")
	fs.StringVar(&opts.FetchProxy, "fetch-proxy", "", "订阅经代理拉取时使用的代理")
	_ = fs.Parse(args)
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return err
	}
	opts.GuardSocket = filepath.Join(opts.DataDir, "no-guard.sock") // 管理服务没有 guard
	var distSrv *dist.Server
	opts.ConnectSrc = func() []string {
		if distSrv == nil {
			return nil
		}
		return distSrv.ConnectSources()
	}
	opts.Extend = func(svc panel.Services) (func(*http.ServeMux, func(http.HandlerFunc) http.Handler), func(*http.ServeMux)) {
		s := &dist.Server{DB: svc.DB, Secrets: svc.Secrets, Nodes: svc.Nodes, Groups: svc.Groups, Dir: filepath.Join(opts.DataDir, "releases"),
			FetchProxy: opts.FetchProxy, Subs: svc.Subs}
		distSrv = s
		// 订阅按各自的更新间隔自动拉取；节点有变化时自动发布并推送（S1.12）。用量提醒每 5 分钟检查一次。
		svc.Subs.OnAutoUpdate = s.OnSubscriptionUpdate
		go svc.Subs.RunScheduler(ctx, logger, s.AfterSubscriptionsUpdated)
		go s.RunAlerts(ctx, logger)
		// Claude / Codex 令牌集中续期：到期前安排一台在线客户端刷新（ADR-071）。
		go s.RunAIRefresh(ctx, logger)
		// AI 账号在线时长：结束已离线客户端的时段（客户端每分钟上报在用的账号）。
		go s.RunAIOnline(ctx, logger)
		// 客户端上报的 Claude Code 遥测单独一个库。打不开时不接收，其他功能照常。
		if store, err := otel.OpenStore(filepath.Join(opts.DataDir, "otel.db")); err != nil {
			logger.Error("open otel store failed; client telemetry uploads are rejected", "err", err)
		} else {
			s.OTel = store
		}
		// 预置配置：数据目录的 seed/ 下放 Mihomo 配置和规则，第一次启动时导入（见 dist.Seed）。
		seedDir := filepath.Join(opts.DataDir, "seed")
		if res, ok, err := s.Seed(ctx, seedDir); err != nil {
			logger.Error("import seed config failed; will retry on next start", "dir", seedDir, "err", err)
		} else if ok {
			logger.Info("imported seed config", "dir", seedDir, "nodes", res.Nodes, "groups", res.Groups, "rules", res.Rules)
		}
		admin := func(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler) {
			s.AdminRoutes(mux, authed, logger)
			mux.Handle("GET /api/v1/dist/info", authed(func(w http.ResponseWriter, _ *http.Request) {
				writeServerInfo(w, opts)
			}))
		}
		return admin, func(mux *http.ServeMux) { s.ClientRoutes(mux, logger) }
	}
	logger.Info("management server", "listen", opts.Listen, "plain_http", opts.PlainHTTP)
	return panel.Run(ctx, opts)
}

// runExportSeed 把管理服务当前的配置导出成预置文件（boxctl server export-seed），
// 用作新部署的初始配置：复制到新服务器数据目录的 seed/ 下，第一次启动时导入。
func runExportSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server export-seed", flag.ExitOnError)
	dataDir := fs.String("data-dir", "/var/lib/banbu-connect", "管理服务的数据目录")
	out := fs.String("out", "seed-export", "输出目录（含节点凭据，不要放进仓库）")
	_ = fs.Parse(args)
	svc, err := panel.OpenServices(ctx, *dataDir, "")
	if err != nil {
		return err
	}
	defer svc.DB.Close()
	s := &dist.Server{DB: svc.DB, Secrets: svc.Secrets, Nodes: svc.Nodes, Groups: svc.Groups}
	res, err := s.ExportSeed(ctx, *out)
	if err != nil {
		return err
	}
	fmt.Printf("已导出到 %s：节点 %d 个、代理组 %d 个、规则 %d 条\n", *out, res.Nodes, res.Groups, res.Rules)
	return nil
}

// writeServerInfo 给「客户端」页面显示绑定需要的信息：证书指纹（自签名时客户端必须填）。
func writeServerInfo(w http.ResponseWriter, opts panel.Options) {
	certFile := opts.TLSCert
	if certFile == "" && !opts.PlainHTTP {
		certFile = filepath.Join(opts.DataDir, "tls", "cert.pem")
	}
	info := map[string]any{"self_signed": opts.TLSCert == "" && !opts.PlainHTTP, "plain_http": opts.PlainHTTP, "fingerprint": ""}
	if b, err := os.ReadFile(certFile); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				sum := sha256.Sum256(block.Bytes)
				info["fingerprint"] = hex.EncodeToString(sum[:])
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = jsonEncode(w, info)
}

func jsonEncode(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}
