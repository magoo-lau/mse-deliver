# mse-deliver 文档索引

| 文档 | 内容 |
|---|---|
| [../README.md](../README.md) | 工具族定位、四工具矩阵、快速构建 |
| [design.md](design.md) | 交付管线设计（最终态）：format:3 套件 / format:1 附件、重打包审计、下载票、双令牌、ADR 摘要 |
| [studio.md](studio.md) | deliver-studio（7860）：打包/重打包/下载票/预设 |
| [console.md](console.md) | deliver-console（7870）：控制面/节点代理、编排 DAG、插件链、冒烟 |

## 拆仓说明

自 ai-mse 单仓（源 commit `cc5410a3`）的 tools/mse-deliver 拆分独立。上游采集器在 **mse-collect-kit 仓**（经 studio REST HTTP 集成，Go 零 import）；租户私有部署语义（tenant-starter）见 mse-framework 仓 `docs/tenancy.md`。历史设计叙事（deliver-design.md ADR 全文）保留在 ai-mse 仓 docs/tech/ 作存档，本仓 docs 为最终态口径。
