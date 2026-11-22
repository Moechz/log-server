// Package onboarding 记录"首次使用向导是否完成"（FR-1005）。
//
// 只存一个小状态文件 <DataDir>/config/onboarding.json；不存任何配置内容。
package onboarding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State 是向导完成状态。
type State struct {
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	CompletedBy string    `json:"completed_by,omitempty"`
	Version     int       `json:"version"`
}

// Load 读取状态；文件不存在/损坏时返回未完成（不报错，避免阻塞启动）。
func Load(path string) State {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return State{}
	}
	return s
}

// Save 原子写入状态（0600；内容不含凭据）。
func Save(path string, s State) error {
	if s.Version == 0 {
		s.Version = 1
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Complete 标记完成。
func Complete(path, actor string) error {
	return Save(path, State{Completed: true, CompletedAt: time.Now().UTC(), CompletedBy: actor, Version: 1})
}

// Reset 删除状态（下次打开应用重新显示向导）。
func Reset(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("重置向导状态失败: %w", err)
	}
	return nil
}
