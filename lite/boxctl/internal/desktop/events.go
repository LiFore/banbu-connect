package desktop

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// eventLog 记下最近的事件（更新失败、续期提醒等），并排好待外壳弹出的系统通知。
type eventLog struct {
	mu      sync.Mutex
	items   []Event
	pending []Notice
	lastID  int64
}

// Event 是一条事件。
type Event struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Notice 是一条待弹出的系统通知。ID 单调递增（纳秒时间戳），进程重启后也不回退，外壳按它去重。
type Notice struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

const (
	maxEvents  = 200
	maxNotices = 20
)

func newEventLog() *eventLog { return &eventLog{} }

func (l *eventLog) record(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, Event{Time: time.Now(), Level: level, Message: msg})
	if len(l.items) > maxEvents {
		l.items = l.items[len(l.items)-maxEvents:]
	}
}

// announce 排一条系统通知，同时记成事件。
func (l *eventLog) announce(kind, title, body string) {
	l.record("info", title+"："+body)
	l.mu.Lock()
	defer l.mu.Unlock()
	id := time.Now().UnixNano()
	if id <= l.lastID {
		id = l.lastID + 1
	}
	l.lastID = id
	l.pending = append(l.pending, Notice{ID: id, Kind: kind, Title: title, Body: body})
	if len(l.pending) > maxNotices {
		l.pending = l.pending[len(l.pending)-maxNotices:]
	}
}

func (l *eventLog) notices() []Notice {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Notice(nil), l.pending...)
}

// list 返回事件，新的在前。
func (l *eventLog) list() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.items))
	for i, e := range l.items {
		out[len(l.items)-1-i] = e
	}
	return out
}

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }
