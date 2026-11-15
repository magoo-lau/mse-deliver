// 内嵌资源：前端 dist（go:embed，构建时 npm build 产出）+ 双平台 agent 二进制（build.sh 先
// 交叉编译 deliver-agent 再嵌入，主机池页提供下载——拷到目标机即装）+ 双平台插件二进制
// （build.sh 交叉编译 deliver-plugins/* 后嵌入，plugin 任务按执行主机 OS 经 /api/plugin/ 分发）。
package main

import (
	"embed"
	"errors"
)

//go:embed all:ui/dist
var uiFS embed.FS

//go:embed agent-bin/deliver-agent-linux agent-bin/deliver-agent-windows.exe
var agentFS embed.FS

const agentBinDir = "agent-bin"

// agentStamp 构建期注入的 agent 版本戳（与内嵌 agent 同 -X 注入）——旧 agent 不识别新任务类型
// （如 docker），编排含新任务时按此预警提示重装 agent；空（go test/dev 构建）=跳过判别
var agentStamp = ""

// agentBinFS 读取内嵌 agent 二进制（占位文件由调用方按大小判别）
func agentBinFS(name string) ([]byte, error) {
	data, err := agentFS.ReadFile(agentBinDir + "/" + name)
	if err != nil {
		return nil, errors.New("未内嵌: " + name)
	}
	return data, nil
}

//go:embed plugin-bin/nacos-config-linux plugin-bin/nacos-config-windows.exe
var pluginFS embed.FS

const pluginBinDir = "plugin-bin"

// pluginBinFS 读取内嵌插件二进制（占位文件由调用方按大小判别）；file 形如 nacos-config-linux
func pluginBinFS(file string) ([]byte, error) {
	data, err := pluginFS.ReadFile(pluginBinDir + "/" + file)
	if err != nil {
		return nil, errors.New("未内嵌插件: " + file)
	}
	return data, nil
}

// pluginFileName 插件名 + 目标 OS → plugin-bin 内文件名（与 build.sh 编译产物命名一致）
func pluginFileName(plugin, os string) string {
	if os == "windows" {
		return plugin + "-windows.exe"
	}
	return plugin + "-linux"
}
