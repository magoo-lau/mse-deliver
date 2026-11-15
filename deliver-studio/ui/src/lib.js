// 共享工具：变量引用扫描 / compose 轻量解析 / 角色与镜像来源元数据 / 单元起步模板 / 客户端校验（与后端同口径）
//
// ---- 术语表（全站唯一口径，UI 文案与后端错误信息同源） ----
// 制品     = 打包素材总称（Harbor 风格），下分三类：应用包 / 镜像包 / 附加文件
// 应用包   = jar / zip / 任意构建产物文件（kind=artifact/jar/ui_zip）
// 镜像包   = docker save 出的 tar（kind=image_tar）
// 镜像地址 = docker 镜像引用 repo/name:tag
// 镜像来源 = 单元三选一：应用包构建（artifact）/ 镜像包导入（image_tar）/ 镜像地址导出（image_ref）
// 镜像名   = 单元应用包构建的产物镜像 repo（tag=版本，字段 imageRepo）
// 镜像仓   = 本地镜像包 tar 仓库（页面「镜像仓」）
// 变量引用 = 模板中的 {{X}}；一切皆「变量」，未登记的叫「未定义引用」
// 内置变量 = UNIT_IMAGE / UNIT_PORT / ARTIFACT_FILE（打包时由引擎替换）
// 打包时 / 安装时 = 变量赋值时机
export const BUILTIN_VARS = [
  { name: 'UNIT_IMAGE', label: '本单元镜像', when: 'build', builtin: true },
  { name: 'UNIT_PORT', label: '本单元端口', when: 'build', builtin: true },
  { name: 'ARTIFACT_FILE', label: '应用包文件名（仅应用包构建的 Dockerfile 可用）', when: 'build', builtin: true }
]

export const ROLE_META = {
  backend: { label: '后端', icon: 'Monitor', color: '#409eff' },
  frontend: { label: '前端', icon: 'Platform', color: '#67c23a' },
  custom: { label: '服务', icon: 'Grid', color: '#909399' }
}

export const SOURCE_META = {
  artifact: { label: '应用包构建', hint: '上传应用包（jar/zip/任意文件）+ Dockerfile 模板，打包时现场构建镜像；zip 自动解压' },
  image_tar: { label: '镜像包导入', hint: '上传 docker save 出的镜像包 tar，直接进包不构建' },
  image_ref: { label: '镜像地址导出', hint: '填镜像地址（如 nginx:1.27），打包时从本机 docker 或镜像仓导出' }
}

export const CATEGORY_META = {
  database: '数据库',
  cache: '缓存',
  middleware: '中间件',
  custom: '自定义'
}

// 扫描文本中的 {{X}} 变量引用（去重保序）
export function scanPlaceholders(texts) {
  const seen = new Set()
  const out = []
  for (const t of [].concat(texts || [])) {
    if (!t) continue
    for (const m of t.matchAll(/\{\{([A-Za-z0-9_]+)\}\}/g)) {
      if (!seen.has(m[1])) {
        seen.add(m[1])
        out.push(m[1])
      }
    }
  }
  return out
}

// 编排块顶层服务名（2 空格缩进 `name:`）
export function composeServiceNames(block) {
  const out = []
  for (const line of String(block || '').split('\n')) {
    const m = line.match(/^  ([A-Za-z0-9_-]+):/)
    if (m && !out.includes(m[1])) out.push(m[1])
  }
  return out
}

// 解析编排块依赖边（depends_on 列表）：[{from: 服务名, to: 服务名}]
export function parseComposeEdges(block) {
  const edges = []
  let cur = null
  let inDeps = false
  for (const line of String(block || '').split('\n')) {
    const svc = line.match(/^  ([A-Za-z0-9_-]+):/)
    if (svc) {
      cur = svc[1]
      inDeps = false
      continue
    }
    if (/^    depends_on:/.test(line)) {
      inDeps = true
      continue
    }
    if (inDeps) {
      const dep = line.match(/^ {6,}-\s*([A-Za-z0-9_.-]+)/)
      if (dep && cur) {
        edges.push({ from: cur, to: dep[1] })
        continue
      }
      if (!/^ {6,}/.test(line) && line.trim() !== '') inDeps = false
    }
  }
  return edges
}

// 镜像地址 → 安全 tar 名（与后端约定一致）
export function safeImageName(ref) {
  return String(ref || '').replace(/[/:]/g, '_')
}

// 应用包类判断（jar/ui_zip 为旧值，兼容 collect-kit 推送与历史数据）
export function isArtifactKind(kind) {
  return kind === 'artifact' || kind === 'jar' || kind === 'ui_zip'
}

// 单元起步模板（向导卡片）：code 由插入处替换 {{CODE}}
export function unitTemplates() {
  return [
    {
      key: 'java',
      title: 'Java 后端（jar）',
      desc: '上传 jar 应用包 + Dockerfile 模板，打包时构建镜像；预置 Spring Boot 服务编排与健康检查',
      unit: {
        name: '后端服务', role: 'backend', source: 'artifact', port: '8080', healthPath: '/actuator/health',
        confDir: '/app/conf', confEnv: { SPRING_CONFIG_ADDITIONAL_LOCATION: 'file:/app/conf/' },
        dockerfile:
          'ARG RUNTIME_IMAGE=eclipse-temurin:21-jre-jammy\nFROM ${RUNTIME_IMAGE}\nENV TZ=Asia/Shanghai JAVA_OPTS="-XX:MaxRAMPercentage=75.0"\nWORKDIR /app\nCOPY {{ARTIFACT_FILE}} app.jar\nEXPOSE 8080\nENTRYPOINT ["sh", "-c", "java $JAVA_OPTS -jar app.jar"]',
        composeTemplate:
          '  {{CODE}}:\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n    environment:\n      TZ: Asia/Shanghai\n    volumes:\n      - ./data/conf:/app/conf\n    ports:\n      - "{{UNIT_PORT}}:{{UNIT_PORT}}"\n'
      }
    },
    {
      key: 'static',
      title: '静态前端（zip）',
      desc: '上传前端 dist zip 应用包，打包时自动解压进构建上下文，用 nginx 模板构建镜像',
      unit: {
        name: '前端站点', role: 'frontend', source: 'artifact', port: '80',
        dockerfile: 'FROM nginx:stable-alpine\nCOPY . /usr/share/nginx/html\nEXPOSE 80\n',
        composeTemplate: '  {{CODE}}:\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n    ports:\n      - "80:80"\n'
      }
    },
    {
      key: 'tar',
      title: '镜像包导入',
      desc: '已有 docker save 出的镜像包 tar，上传后直接进包',
      unit: {
        name: '镜像服务', role: 'custom', source: 'image_tar',
        composeTemplate: '  {{CODE}}:\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n'
      }
    },
    {
      key: 'ref',
      title: '镜像地址导出',
      desc: '如 nginx:1.27 / redis:7——打包时从本机 docker 或镜像仓导出',
      unit: {
        name: '镜像服务', role: 'custom', source: 'image_ref', imageRef: 'nginx:stable-alpine',
        composeTemplate: '  {{CODE}}:\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n'
      }
    },
    {
      key: 'blank',
      title: '空白单元',
      desc: '不预填任何内容，全部自己定义',
      unit: {
        name: '服务单元', role: 'custom', source: 'artifact',
        composeTemplate: '  {{CODE}}:\n    image: {{UNIT_IMAGE}}\n    restart: unless-stopped\n'
      }
    }
  ]
}

// 按角色起一个应用内不重复的单元编码
export function nextUnitCode(units, role) {
  const taken = new Set((units || []).map((u) => u.code))
  const prefer = { backend: 'app', frontend: 'web', custom: 'svc' }[role] || 'svc'
  let code = prefer
  let n = 2
  while (taken.has(code)) code = prefer + '-' + n++
  return code
}

// ---- YAML 顶层键分段（YamlSectionsEditor 双视图共用） ----
// splitYamlSections 把 YAML 文本按顶层键切成 {preamble, sections:[{key, block}]}；
// block 为该键下的缩进文本（不含键行，尾部空行去除）；前导注释/分隔行保留在 preamble。
export function splitYamlSections(text) {
  const lines = String(text || '').split('\n')
  const preamble = []
  const sections = []
  let cur = null
  for (const line of lines) {
    const m = line.match(/^([A-Za-z0-9_.-]+):\s*(#.*)?$/)
    if (m) {
      cur = { key: m[1], lines: [] }
      sections.push(cur)
      continue
    }
    if (cur) cur.lines.push(line)
    else preamble.push(line)
  }
  const out = sections.map((s) => ({ key: s.key, block: s.lines.join('\n').replace(/\s+$/, '') }))
  return { preamble: preamble.join('\n').replace(/\s+$/, ''), sections: out }
}

// joinYamlSections 重建 YAML 文本（preamble + 各 `key:\n block` 段）
export function joinYamlSections(preamble, sections) {
  const parts = []
  const pre = String(preamble || '').trim()
  if (pre) parts.push(pre)
  for (const s of sections || []) {
    const block = String(s.block || '').replace(/\s+$/, '')
    if (!s.key && !block) continue
    parts.push(block ? `${s.key}:\n${block}` : `${s.key}:`)
  }
  return parts.join('\n') + (parts.length ? '\n' : '')
}

// 应用定义客户端校验（与后端 validateApp 同口径，错误信息中文）
export function validateAppClient(app) {
  if (!/^[a-z][a-z0-9-]*$/.test(app.code || '')) return '应用编码须为小写字母数字短横线（字母开头），如 demo-app'
  if (!app.units || !app.units.length) return '至少需要一个服务单元'
  const codes = new Set()
  const services = new Map()
  let entry = 0
  for (const u of app.units) {
    if (!/^[a-z][a-z0-9-]*$/.test(u.code || '')) return `单元编码须为 kebab 形态: ${u.code}`
    if (codes.has(u.code)) return `单元编码重复: ${u.code}`
    codes.add(u.code)
    if (u.entrypoint) entry++
    if (!String(u.composeTemplate || '').trim()) return `单元 ${u.code} 缺少编排服务块`
    for (const line of String(u.composeTemplate).split('\n')) {
      const t = line.replace(/^ +/, '')
      if (!t || t.startsWith('#')) continue
      const indent = line.length - t.length
      if (indent < 2 || indent % 2 !== 0) return `单元 ${u.code} 编排块缩进须为 2 空格倍数: ${line}`
    }
    for (const svc of composeServiceNames(u.composeTemplate)) {
      if (services.has(svc)) return `compose 服务名 ${svc} 重复（${services.get(svc)} 与 ${u.code}）`
      services.set(svc, u.code)
    }
  }
  if (entry > 1) return '主入口单元只能标记一个'
  const names = new Set()
  for (const v of app.variables || []) {
    if (!/^[A-Z][A-Z0-9_]*$/.test(v.name || '')) return `变量名须为大写蛇形（如 TENANT_ID）: ${v.name}`
    if (names.has(v.name)) return `变量重复定义: ${v.name}`
    names.add(v.name)
  }
  return ''
}
