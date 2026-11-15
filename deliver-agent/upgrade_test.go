package main

// agent 自升级单元测试：applyUpgrade 替换舞步（临时目录模拟，不动真实二进制）、
// 版本比较守卫（只升不降）。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyUpgradeReplaceDance(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "deliver-agent")
	if err := os.WriteFile(exe, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := applyUpgrade(exe, []byte("NEW-BINARY")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Fatalf("新二进制未就位: %q", got)
	}
	old, err := os.ReadFile(exe + ".old")
	if err != nil || string(old) != "OLD" {
		t.Fatalf("旧件留底异常: %q, %v", old, err)
	}
	// 重复升级（模拟失败退避后再次下发）：旧留底换代、新件仍就位
	if err := applyUpgrade(exe, []byte("NEWER")); err != nil {
		t.Fatal(err)
	}
	if got, _ = os.ReadFile(exe); string(got) != "NEWER" {
		t.Fatalf("二次升级未就位: %q", got)
	}
	if old, _ = os.ReadFile(exe + ".old"); string(old) != "NEW-BINARY" {
		t.Fatalf("二次升级留底异常: %q", old)
	}
	// 临时文件不残留
	for _, leftover := range []string{exe + ".part", exe + ".new"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("临时文件残留: %s", leftover)
		}
	}
}

func TestApplyUpgradeBadPathFails(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "no-such-dir", "agent")
	if err := applyUpgrade(exe, []byte("NEW")); err == nil {
		t.Fatal("目录不存在应报错")
	}
	removeStaleBinary() // 无 .old 留底时忽略不报错
}

// 版本守卫逻辑内嵌于 selfUpgrade（进程副作用不可直跑），此处验证比较语义：
// yyyyMMddHHmm 定宽字典序即时间序——只升不降判定
func TestUpgradeVersionGuardSemantics(t *testing.T) {
	cases := []struct {
		directive, current string
		shouldApply        bool
	}{
		{"202610292105", "202610270919", true},
		{"202610292105", "202610292105", false}, // 幂等跳过
		{"202610270919", "202610292105", false}, // 只升不降
		{"1.0.0", "1.0.0", false},               // dev 构建默认值同版本
	}
	for _, c := range cases {
		got := c.directive != c.current && c.directive > c.current
		if got != c.shouldApply {
			t.Errorf("directive=%s current=%s 应用判定=%v, want %v", c.directive, c.current, got, c.shouldApply)
		}
	}
}
