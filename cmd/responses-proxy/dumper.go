package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

// RequestDumper 负责将每个接入的请求与转换体落盘保存，方便排查上游 400/500/超时等异常。
type RequestDumper struct {
	dir     string
	counter atomic.Uint64
}

type DumpRecord struct {
	ID                string          `json:"id"`
	Timestamp         string          `json:"timestamp"`
	Endpoint          string          `json:"endpoint"`
	ClientModel       string          `json:"client_model"`
	UpstreamModel     string          `json:"upstream_model"`
	RawClientBody     json.RawMessage `json:"raw_client_body"`
	ConvertedChatBody json.RawMessage `json:"converted_chat_body,omitempty"`
	StatusCode        int             `json:"status_code"`
	Error             string          `json:"error,omitempty"`
	DurationMs        int64           `json:"duration_ms"`
}

func NewRequestDumper(dir string) *RequestDumper {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[dumper] failed to create dump dir %s: %v", dir, err)
		return nil
	}
	log.Printf("[dumper] request dumping enabled -> %s", dir)
	return &RequestDumper{dir: dir}
}

// NextID 生成单调递增的请求唯一标识。
func (d *RequestDumper) NextID() string {
	if d == nil {
		return ""
	}
	seq := d.counter.Add(1)
	return fmt.Sprintf("req_%s_%04d", time.Now().Format("20060102_150405"), seq)
}

// Record 异步保存请求记录到磁盘，永不阻塞主调用流。
func (d *RequestDumper) Record(rec DumpRecord) {
	if d == nil {
		return
	}
	go func() {
		raw, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return
		}

		// 1. 始终更新 latest_request.json
		_ = os.WriteFile(filepath.Join(d.dir, "latest_request.json"), raw, 0644)

		// 2. 如果是报错请求（status >= 400 或存在错误信息），专门保存独立错误凭证
		if rec.StatusCode >= 400 || rec.Error != "" {
			errFilename := fmt.Sprintf("error_%s_%d.json", rec.ID, rec.StatusCode)
			errPath := filepath.Join(d.dir, errFilename)
			_ = os.WriteFile(errPath, raw, 0644)
			_ = os.WriteFile(filepath.Join(d.dir, "latest_error.json"), raw, 0644)
			log.Printf("[dumper] error request dumped to %s", errPath)

			// 清理旧的错误转储（最多保留最近 50 个文件）
			d.pruneOldFiles("error_*.json", 50)
		}
	}()
}

func (d *RequestDumper) pruneOldFiles(pattern string, maxKeep int) {
	matches, err := filepath.Glob(filepath.Join(d.dir, pattern))
	if err != nil || len(matches) <= maxKeep {
		return
	}
	sort.Strings(matches)
	for i := 0; i < len(matches)-maxKeep; i++ {
		_ = os.Remove(matches[i])
	}
}
