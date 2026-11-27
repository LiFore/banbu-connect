package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortSock 返回较短的 socket 路径，避免超过 unix socket 路径长度限制。
func shortSock(t *testing.T) string {
	dir, err := os.MkdirTemp("", "bx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func startServer(t *testing.T, handlers map[string]Handler) string {
	path := shortSock(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	srv := &Server{Path: path, Handlers: handlers}
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	// 等待 socket 就绪
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCallRoundTrip(t *testing.T) {
	path := startServer(t, map[string]Handler{
		"echo": func(_ context.Context, p json.RawMessage) (any, error) {
			var in map[string]string
			if err := json.Unmarshal(p, &in); err != nil {
				return nil, err
			}
			return map[string]string{"got": in["v"]}, nil
		},
		"fail": func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("boom")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var out map[string]string
	if err := Call(ctx, path, "echo", map[string]string{"v": "hi"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["got"] != "hi" {
		t.Fatalf("got %v", out)
	}
	if err := Call(ctx, path, "fail", nil, nil); err == nil || err.Error() != "boom" {
		t.Fatalf("want boom, got %v", err)
	}
	if err := Call(ctx, path, "nope", nil, nil); err == nil {
		t.Fatal("want unknown method error")
	}
}

func TestCallUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := Call(ctx, shortSock(t), "status", nil, nil)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}
