# deliver-studio（7860）—— 唯一打包引擎

## 职责

1. **套件打包**：制品 + 预设 → format:3 套件（docker build 逐服务镜像 / fat-jar / UI zip；打包向导按 preset params 动态渲染表单）。
2. **重打包**：套件 + 交付附件 → 最终交付包（附件 format:1 入库全量校验 sha 幂等；manifest 硬校验 + extras/ 冲突硬拦；重建外层 manifest 审计段）。
3. **接收采集**：collect-kit（mse-collect-kit 仓）经 REST 推送制品/镜像/预设（登录鉴权；同名同大小去重复用）。
4. **分发**：套件/交付包下载——GB 级包走一次性下载票（`POST /api/packages/{id}/ticket` → 浏览器原生下载，流式落盘）。

## 构建 / 运行 / 测试

```sh
sh ../build.sh studio            # 或本目录 build.sh / build.ps1 / build.cmd
./deliver-studio                 # 默认 0.0.0.0:7860（控制台 http://localhost:7860）
go test ./...                    # 含打包/重打包/下载票/鉴权用例（回归锚点：TestPackageDownloadTicket、
                                 # TestBuildPlanZipArtifactWithWholeFileRef 等）
```

- 内嵌：`go:embed presets`（ai-mse.json / demo.json）+ ui/dist（构建前先 npm build ui）。
- 产物：deliver-studio.exe / deliver-studio-linux（本目录，gitignore）。
- presets 是 collect-kit 仓 recipes/ai-mse/studio-preset.json 的同步副本（字节一致）。

## 关键口径

- 打包前置拦截：应用包选 zip 且模板含 {{ARTIFACT_FILE}} → 硬拒（指路：后端=jar/fat-jar，前端=UI zip 配 COPY .）。
- 单机部署选项：随包附带 install.sh 薄壳（configCenter 组合打包硬拦截）。
- 交付附件直推：`mse.deliver.studio.*` 配置可让平台签发侧直推 studio（不走人工上传）。
