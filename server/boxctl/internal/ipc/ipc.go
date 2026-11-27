// Package ipc 实现 panel 与 guard 之间的 unix socket 通信。
//
// 协议：每个连接一次请求一次响应，均为单行 JSON。
//
//	→ {"method":"status","params":{...}}
//	← {"result":{...}} 或 {"error":"..."}
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Handler 处理一个方法调用，返回值会被序列化为 result。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Server struct {
	Path     string
	Handlers map[string]Handler
	Logger   *slog.Logger
}

// Serve 监听 socket 直到 ctx 结束。启动前会清理遗留的 socket 文件。
func (s *Server) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return err
	}
	_ = os.Remove(s.Path)
	ln, err := net.Listen("unix", s.Path)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.Path, 0o660); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				_ = os.Remove(s.Path)
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	var req Request
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else if h, ok := s.Handlers[req.Method]; !ok {
		resp.Error = "unknown method: " + req.Method
	} else if result, err := h(ctx, req.Params); err != nil {
		resp.Error = err.Error()
	} else if resp.Result, err = json.Marshal(result); err != nil {
		resp.Error = "marshal result: " + err.Error()
	}
	if err := json.NewEncoder(conn).Encode(resp); err != nil && s.Logger != nil {
		s.Logger.Warn("ipc write response", "method", req.Method, "err", err)
	}
}

// ErrUnreachable 表示对端没有运行或 socket 不可连接。
var ErrUnreachable = errors.New("ipc: peer unreachable")

// Call 调用对端方法，结果解码到 out（可为 nil）。
func Call(ctx context.Context, path, method string, params, out any) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	req := Request{Method: method}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if out != nil {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}
