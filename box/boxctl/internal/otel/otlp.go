// Package otel 是 Claude Code 遥测（OpenTelemetry）的本机接收与统计：
// OTLP/HTTP（http/json）接收器只监听本机，数据存在本机 SQLite，不上传。
// 写入 Claude Code 配置的事由托盘外壳负责（它运行在用户会话里，后台服务以 root/SYSTEM 运行）。
// 参考 open-agentcode 的 packages/core/src/otel。
package otel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// Attrs 是展开后的属性。
type Attrs map[string]any

// 单个字符串属性的长度上限，防止异常大的内容撑爆数据库。
const maxString = 64 << 10

// num 兼容 OTLP JSON 里写成字符串或数字的 64 位整数（intValue、timeUnixNano 等）。
type num string

func (n *num) UnmarshalJSON(b []byte) error {
	*n = num(strings.Trim(string(b), `"`))
	return nil
}

func (n num) float() float64 {
	f, _ := strconv.ParseFloat(string(n), 64)
	return f
}

// div 把纳秒整数除以 d（取整），大数用 big.Int 避免精度丢失。
func (n num) div(d int64) int64 {
	if n == "" || n == "null" {
		return 0
	}
	v, ok := new(big.Int).SetString(string(n), 10)
	if !ok {
		return int64(n.float()) / d
	}
	return v.Quo(v, big.NewInt(d)).Int64()
}

func (n num) ms() int64 { return n.div(1_000_000) }
func (n num) us() int64 { return n.div(1_000) }

type anyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *num     `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	ArrayValue  *struct {
		Values []anyValue `json:"values,omitempty"`
	} `json:"arrayValue,omitempty"`
	KvlistValue *struct {
		Values []keyValue `json:"values,omitempty"`
	} `json:"kvlistValue,omitempty"`
	BytesValue *string `json:"bytesValue,omitempty"`
}

type keyValue struct {
	Key   string    `json:"key"`
	Value *anyValue `json:"value,omitempty"`
}

func (v *anyValue) value() any {
	switch {
	case v == nil:
		return nil
	case v.StringValue != nil:
		s := *v.StringValue
		if len(s) > maxString {
			s = s[:maxString]
		}
		return s
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		return v.IntValue.float()
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for i := range v.ArrayValue.Values {
			out = append(out, v.ArrayValue.Values[i].value())
		}
		return out
	case v.KvlistValue != nil:
		return attrsOf(v.KvlistValue.Values)
	case v.BytesValue != nil:
		return *v.BytesValue
	}
	return nil
}

func attrsOf(list []keyValue) Attrs {
	out := Attrs{}
	for _, kv := range list {
		out[kv.Key] = kv.Value.value()
	}
	return out
}

func merge(a, b Attrs) Attrs {
	out := make(Attrs, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// contentKeys 是提示词和助手回复的原文。不收集：Claude Code 侧没开 OTEL_LOG_USER_PROMPTS /
// OTEL_LOG_ASSISTANT_RESPONSES，这里再兜底删一次（用户自己开了、或别的程序发来的数据）。
// 长度（prompt_length、response_length）保留。
var contentKeys = []string{"prompt", "user_prompt", "response", "assistant_response"}

// scrub 删除属性里提示词和助手回复的原文，返回同一个 map。
func scrub(a Attrs) Attrs {
	for _, k := range contentKeys {
		delete(a, k)
	}
	return a
}

func sessionOf(a Attrs) string {
	s, _ := a["session.id"].(string)
	return s
}

func hashOf(parts ...any) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type resource struct {
	Attributes []keyValue `json:"attributes,omitempty"`
}

// ---- traces ----

// SpanRow 是一条 span。
type SpanRow struct {
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Name         string `json:"name"`
	StartUs      int64  `json:"start_us"`
	EndUs        int64  `json:"end_us"`
	Status       int    `json:"status"` // 0 UNSET / 1 OK / 2 ERROR
	SessionID    string `json:"session_id,omitempty"`
	Attrs        Attrs  `json:"attrs"`
}

type rawSpan struct {
	TraceID           string `json:"traceId"`
	SpanID            string `json:"spanId"`
	ParentSpanID      string `json:"parentSpanId"`
	Name              string `json:"name"`
	StartTimeUnixNano num    `json:"startTimeUnixNano"`
	EndTimeUnixNano   num    `json:"endTimeUnixNano"`
	Status            struct {
		Code int `json:"code"`
	} `json:"status"`
	Attributes []keyValue `json:"attributes"`
	Events     []struct {
		Name         string     `json:"name"`
		TimeUnixNano num        `json:"timeUnixNano"`
		Attributes   []keyValue `json:"attributes"`
	} `json:"events"`
}

// ParseTraces 把 ExportTraceServiceRequest 展开为 span 行。
func ParseTraces(body []byte) ([]SpanRow, error) {
	var req struct {
		ResourceSpans []struct {
			Resource   resource `json:"resource"`
			ScopeSpans []struct {
				Spans []rawSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	var rows []SpanRow
	for _, rs := range req.ResourceSpans {
		res := attrsOf(rs.Resource.Attributes)
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				attrs := scrub(merge(res, attrsOf(sp.Attributes)))
				if len(sp.Events) > 0 {
					evs := make([]any, 0, len(sp.Events))
					for _, e := range sp.Events {
						ev := scrub(merge(Attrs{"name": e.Name, "tsMs": e.TimeUnixNano.ms()}, attrsOf(e.Attributes)))
						evs = append(evs, ev)
					}
					attrs["span.events"] = evs
				}
				rows = append(rows, SpanRow{
					TraceID: sp.TraceID, SpanID: sp.SpanID, ParentSpanID: sp.ParentSpanID, Name: sp.Name,
					StartUs: sp.StartTimeUnixNano.us(), EndUs: sp.EndTimeUnixNano.us(), Status: sp.Status.Code,
					SessionID: sessionOf(attrs), Attrs: attrs,
				})
			}
		}
	}
	return rows, nil
}

// ---- logs（Claude Code 的事件：user_prompt、api_request、tool_result 等）----

// LogRow 是一条事件。
type LogRow struct {
	Hash      string `json:"hash"`
	TsMs      int64  `json:"ts_ms"`
	Name      string `json:"name"` // 去掉 claude_code. 前缀的事件名
	SessionID string `json:"session_id,omitempty"`
	Attrs     Attrs  `json:"attrs"`
}

type rawLog struct {
	TimeUnixNano         num        `json:"timeUnixNano"`
	ObservedTimeUnixNano num        `json:"observedTimeUnixNano"`
	EventName            string     `json:"eventName"`
	Body                 *anyValue  `json:"body"`
	Attributes           []keyValue `json:"attributes"`
}

// ParseLogs 把 ExportLogsServiceRequest 展开为事件行。
func ParseLogs(body []byte) ([]LogRow, error) {
	var req struct {
		ResourceLogs []struct {
			Resource  resource `json:"resource"`
			ScopeLogs []struct {
				LogRecords []rawLog `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	var rows []LogRow
	for _, rl := range req.ResourceLogs {
		res := attrsOf(rl.Resource.Attributes)
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				attrs := scrub(merge(res, attrsOf(rec.Attributes)))
				name, _ := attrs["event.name"].(string)
				if name == "" {
					name = rec.EventName
				}
				if name == "" {
					name, _ = rec.Body.value().(string)
				}
				ts := rec.TimeUnixNano.ms()
				if ts == 0 {
					ts = rec.ObservedTimeUnixNano.ms()
				}
				rows = append(rows, LogRow{
					// 按去掉内容后的属性去重：库里和上报的数据都不含原文，连原文的摘要也不留。
					Hash: hashOf(attrs, rec.TimeUnixNano, rec.ObservedTimeUnixNano, name), TsMs: ts, Name: strings.TrimPrefix(name, "claude_code."),
					SessionID: sessionOf(attrs), Attrs: attrs,
				})
			}
		}
	}
	return rows, nil
}

// ---- metrics ----

// MetricRow 是一个数据点。直方图的 Value 记 sum，次数和最值放进 Attrs。
type MetricRow struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	StartMs    int64   `json:"start_ms"`
	EndMs      int64   `json:"end_ms"`
	Value      float64 `json:"value"`
	Cumulative bool    `json:"cumulative"`
	SessionID  string  `json:"session_id,omitempty"`
	Attrs      Attrs   `json:"attrs"`
}

type rawPoint struct {
	StartTimeUnixNano num        `json:"startTimeUnixNano"`
	TimeUnixNano      num        `json:"timeUnixNano"`
	AsDouble          *float64   `json:"asDouble,omitempty"`
	AsInt             *num       `json:"asInt,omitempty"`
	Attributes        []keyValue `json:"attributes"`
	Count             *num       `json:"count,omitempty"`
	Sum               *float64   `json:"sum,omitempty"`
	Min               *float64   `json:"min,omitempty"`
	Max               *float64   `json:"max,omitempty"`
}

type pointSet struct {
	DataPoints             []rawPoint `json:"dataPoints"`
	AggregationTemporality int        `json:"aggregationTemporality"` // 1 DELTA，2 CUMULATIVE
}

// ParseMetrics 把 ExportMetricsServiceRequest 展开为数据点。
func ParseMetrics(body []byte) ([]MetricRow, error) {
	var req struct {
		ResourceMetrics []struct {
			Resource     resource `json:"resource"`
			ScopeMetrics []struct {
				Metrics []struct {
					Name                 string    `json:"name"`
					Unit                 string    `json:"unit"`
					Sum                  *pointSet `json:"sum,omitempty"`
					Gauge                *pointSet `json:"gauge,omitempty"`
					Histogram            *pointSet `json:"histogram,omitempty"`
					ExponentialHistogram *pointSet `json:"exponentialHistogram,omitempty"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	var rows []MetricRow
	for _, rm := range req.ResourceMetrics {
		res := attrsOf(rm.Resource.Attributes)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				set, histogram := m.Sum, false
				switch {
				case set != nil:
				case m.Gauge != nil:
					set = m.Gauge
				case m.Histogram != nil:
					set, histogram = m.Histogram, true
				case m.ExponentialHistogram != nil:
					set, histogram = m.ExponentialHistogram, true
				default:
					continue
				}
				for _, p := range set.DataPoints {
					attrs := scrub(merge(res, attrsOf(p.Attributes)))
					if m.Unit != "" {
						attrs["metric.unit"] = m.Unit
					}
					var v float64
					switch {
					case histogram:
						if p.Sum != nil {
							v = *p.Sum
						}
						if p.Count != nil {
							attrs["histogram.count"] = p.Count.float()
						}
						attrs["histogram.min"], attrs["histogram.max"] = p.Min, p.Max
					case p.AsDouble != nil:
						v = *p.AsDouble
					case p.AsInt != nil:
						v = p.AsInt.float()
					}
					rows = append(rows, MetricRow{
						Hash: hashOf(res, m.Name, p), Name: m.Name, StartMs: p.StartTimeUnixNano.ms(), EndMs: p.TimeUnixNano.ms(),
						Value: v, Cumulative: set.AggregationTemporality == 2, SessionID: sessionOf(attrs), Attrs: attrs,
					})
				}
			}
		}
	}
	return rows, nil
}

func decode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	return d.Decode(v)
}
