// 内嵌示例预设守门：全部预设（presets/*.json）解析+校验通过（applyPreset 同口径），
// 且各预设之间应用/组件编码不冲突（组件目录按编码 upsert，重名会互相覆盖）。
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedPresetsValid(t *testing.T) {
	presets, err := embeddedPresets()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := presets["demo"]; !ok {
		t.Fatal("内嵌示例预设缺 demo（通用教学样例）")
	}
	if _, ok := presets["ai-mse"]; !ok {
		t.Fatal("内嵌示例预设缺 ai-mse")
	}
	appSeen := map[string]string{} // 编码 → 所属预设
	compSeen := map[string]string{}
	for name, data := range presets {
		var p Preset
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("预设 %s 解析失败: %v", name, err)
		}
		for i := range p.Components {
			if err := validateComponent(&p.Components[i]); err != nil {
				t.Errorf("预设 %s 组件 %s: %v", name, p.Components[i].Code, err)
			}
			if prev, ok := compSeen[p.Components[i].Code]; ok {
				t.Errorf("组件编码 %s 在预设 %s 与 %s 间冲突（upsert 会互相覆盖）", p.Components[i].Code, prev, name)
			}
			compSeen[p.Components[i].Code] = name
		}
		for i := range p.Apps {
			if err := validateApp(&p.Apps[i]); err != nil {
				t.Errorf("预设 %s 应用 %s: %v", name, p.Apps[i].Code, err)
			}
			if prev, ok := appSeen[p.Apps[i].Code]; ok {
				t.Errorf("应用编码 %s 在预设 %s 与 %s 间冲突", p.Apps[i].Code, prev, name)
			}
			appSeen[p.Apps[i].Code] = name
		}
	}
}

// demo-app 必须声明附件四件套（附件链教学样例的立身之本，防回退）
func TestDemoPresetAttachmentVars(t *testing.T) {
	presets, err := embeddedPresets()
	if err != nil {
		t.Fatal(err)
	}
	var p Preset
	if err := json.Unmarshal(presets["demo"], &p); err != nil {
		t.Fatal(err)
	}
	var app *App
	for i := range p.Apps {
		if p.Apps[i].Code == "demo-app" {
			app = &p.Apps[i]
		}
	}
	if app == nil {
		t.Fatal("demo 预设缺 demo-app")
	}
	got := []string{}
	for _, v := range app.Variables {
		if v.When != "install" {
			continue
		}
		got = append(got, v.Name)
	}
	sort.Strings(got)
	want := "LICENSE_PUBLIC_KEY,TENANT_CODE,TENANT_ID,TENANT_NAME"
	gotCSV := got[0]
	for _, g := range got[1:] {
		gotCSV += "," + g
	}
	if gotCSV != want {
		t.Fatalf("demo-app 安装变量应为附件四件套 [%s]，实际 [%s]", want, gotCSV)
	}
	// 配置模板引用四件套（模板接线示范）
	for _, name := range app.Variables {
		if !strings.Contains(app.Units[0].ConfigTemplate, "{{"+name.Name+"}}") {
			t.Errorf("demo-app 配置模板未引用 %s", name.Name)
		}
	}
}
