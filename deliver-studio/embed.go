// deliver-studio 内嵌资产：Vue 前端 dist（单文件即开即用）+ 内置示例预设。
// 占位文件随库提交保证 go:embed 恒可编译；正式构建用 build.sh（npm build + go build）覆写。
package main

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed all:ui/dist
var uiFS embed.FS

//go:embed presets
var presetFS embed.FS

// embeddedPresets 列出内嵌示例预设（presets/*.json），供「一键载入」。
// 权威源在各仓库采集器（如 ai-mse 的 collect-kit recipes/ai-mse/studio-preset.json），此处是随二进制分发的同步副本。
func embeddedPresets() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(presetFS, "presets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		data, err := presetFS.ReadFile(path)
		if err != nil {
			return err
		}
		out[strings.TrimSuffix(strings.TrimPrefix(path, "presets/"), ".json")] = data
		return nil
	})
	return out, err
}
