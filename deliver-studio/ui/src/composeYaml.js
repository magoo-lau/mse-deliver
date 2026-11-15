// 编排服务块 YAML 工具：解析 / 序列化 / 手术式 depends_on 编辑
// 编排块格式 = docker-compose services: 下的 2 空格缩进服务名顶层键文本（与打包引擎拼接口径一致，见 engine.go assemble）。
// 原则：compose 文本是唯一事实源——图形操作只做最小文本手术（保格式、保 {{X}} 原样），整段重排仅在属性面板编辑该服务时发生。
import yaml from 'js-yaml'

// {{X}} 裸写法会被 YAML 当嵌套 flow mapping 解析（不报错但得到垃圾结构），须先换成私有区字符标记再 load
const PH = '\uE000'
const PH_TEXT_RE = /\{\{(\w+)\}\}/g
const PH_MARK_RE = new RegExp(PH + '(\\w+)' + PH, 'g')

const DUMP_OPTS = { indent: 2, lineWidth: -1, noRefs: true }

function protectStr(s) {
  return s.replace(PH_TEXT_RE, PH + '$1' + PH)
}
function restoreStr(s) {
  return s.replace(PH_MARK_RE, '{{$1}}')
}
function deepRestore(v) {
  if (typeof v === 'string') return restoreStr(v)
  if (Array.isArray(v)) return v.map(deepRestore)
  if (v && typeof v === 'object') {
    for (const k of Object.keys(v)) v[k] = deepRestore(v[k])
    return v
  }
  return v
}
function deepProtect(v) {
  if (typeof v === 'string') return protectStr(v)
  if (Array.isArray(v)) return v.map(deepProtect)
  if (v && typeof v === 'object') {
    const o = {}
    for (const k of Object.keys(v)) o[k] = deepProtect(v[k])
    return o
  }
  return v
}

// 解析编排块 → {服务名: 服务体}；空块返回 {}；非法（解析失败/结构不符）返回 null
export function parseBlock(text) {  const t = String(text || '')
  if (!t.trim()) return {}
  let doc
  try {
    doc = yaml.load(protectStr('services:\n' + t))
  } catch {
    return null
  }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) return null
  const svcs = doc.services
  if (svcs == null) return {}
  if (typeof svcs !== 'object' || Array.isArray(svcs)) return null
  const out = {}
  for (const [k, v] of Object.entries(svcs)) {
    if (v != null && (typeof v !== 'object' || Array.isArray(v))) return null
    out[k] = deepRestore(v == null ? {} : JSON.parse(JSON.stringify(v)))
  }
  return out
}

// 服务块 map → 编排块文本（整段规范化重排：仅属性面板编辑该服务时使用）
export function dumpBlock(model) {
  if (!model || !Object.keys(model).length) return ''
  const dumped = yaml.dump({ services: deepProtect(JSON.parse(JSON.stringify(model))) }, DUMP_OPTS)
    .replace(PH_MARK_RE, '{{$1}}')
  // 只剥 services: 头行，其余行保持缩进原样（服务名即 2 空格缩进）
  return dumped
    .split('\n')
    .filter((l) => l !== 'services:')
    .join('\n')
    .replace(/\n+$/, '\n')
}

// 模型内依赖边：depends_on 兼容块列表 / flow 列表 / 对象（condition 形态取键）三种写法
export function modelEdges(model) {
  const edges = []
  for (const [svc, body] of Object.entries(model || {})) {
    const deps = body && body.depends_on
    if (deps == null) continue
    if (Array.isArray(deps)) {
      for (const d of deps) if (typeof d === 'string') edges.push({ from: svc, to: d })
    } else if (typeof deps === 'string') {
      edges.push({ from: svc, to: deps })
    } else if (typeof deps === 'object') {
      for (const d of Object.keys(deps)) edges.push({ from: svc, to: d })
    }
  }
  return edges
}

// ---- 文本手术：段落切分（与 lib.js composeServiceNames 同口径：2 空格缩进服务名键行） ----

const SEG_HEAD_RE = /^ {2}([A-Za-z0-9_.-]+):/

function splitSegments(text) {
  const lines = String(text || '').split('\n')
  const headIdx = []
  lines.forEach((line, i) => {
    if (SEG_HEAD_RE.test(line) && !line.startsWith('    ')) headIdx.push(i)
  })
  const preamble = headIdx.length ? lines.slice(0, headIdx[0]) : lines.slice()
  const segs = headIdx.map((start, si) => {
    const end = si + 1 < headIdx.length ? headIdx[si + 1] : lines.length
    return { name: lines[start].match(SEG_HEAD_RE)[1], start, end }
  })
  return { lines, preamble, segs }
}

function joinSegments(lines, preamble, segs, overrides) {
  // overrides: Map<段名, string[]>（替换该段全部行：段头+体）
  const parts = []
  const used = new Set()
  for (const p of preamble) parts.push(p)
  for (const seg of segs) {
    const ov = overrides.get(seg.name)
    if (ov) {
      parts.push(...ov)
      used.add(seg.name)
    } else {
      for (let i = seg.start; i < seg.end; i++) parts.push(lines[i])
    }
  }
  return parts.join('\n')
}

function segBody(lines, seg) {
  return lines.slice(seg.start + 1, seg.end)
}

function segHeadLine(lines, seg) {
  return lines[seg.start]
}

// 单服务规范化重排为段文本（含 2 空格段头缩进）
function dumpSegment(name, body) {
  const dumped = yaml.dump(deepProtect({ [name]: JSON.parse(JSON.stringify(body || {})) }), DUMP_OPTS)
    .replace(PH_MARK_RE, '{{$1}}')
  return dumped
    .split('\n')
    .filter((l) => l.trim() !== '')
    .map((l) => (l.trim() === '' ? l : '  ' + l))
    .join('\n')
}

const esc = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

function depListLineRe(name) {
  return new RegExp(`^ {6}-\\s*${esc(name)}\\s*$`)
}

// depends_on 写入（from 段追加 to，已存在则原样返回）；from 不存在返回 null；
// 对象形态（condition）降级为整段重排；文本不合法（解析失败）返回 null
export function addDependency(text, from, to) {
  const t = String(text || '')
  const { lines, preamble, segs } = splitSegments(t)
  const seg = segs.find((s) => s.name === from)
  if (!seg) return null
  const body = segBody(lines, seg)
  const depIdx = body.findIndex((l) => /^ {4}depends_on:/.test(l))
  if (depIdx < 0) {
    body.unshift('    depends_on:', `      - ${to}`)
    return joinSegments(lines, preamble, segs, new Map([[from, [segHeadLine(lines, seg), ...body]]]))
  }
  const depLine = body[depIdx]
  const flow = depLine.match(/^ {4}depends_on:\s*\[(.*)\]\s*$/)
  if (flow) {
    const list = flow[1].split(',').map((x) => x.trim().replace(/^['"]|['"]$/g, '')).filter(Boolean)
    if (list.includes(to)) return t
    list.push(to)
    body[depIdx] = `    depends_on: [${list.join(', ')}]`
    return joinSegments(lines, preamble, segs, new Map([[from, [segHeadLine(lines, seg), ...body]]]))
  }
  // 块列表 / 对象形态：对象形态下一段行是 6 空格键（非列表项）
  const nextLine = body[depIdx + 1] || ''
  const isObjectForm = /^ {6}[A-Za-z0-9_.-]+:/.test(nextLine) && !/^ {6}-/.test(nextLine)
  if (isObjectForm) return rewriteServiceDep(text, from, to, 'add')
  if (body.some((l) => depListLineRe(to).test(l))) return t
  let last = depIdx
  for (let i = depIdx + 1; i < body.length && /^ {6,}\S/.test(body[i]); i++) last = i
  body.splice(last + 1, 0, `      - ${to}`)
  return joinSegments(lines, preamble, segs, new Map([[from, [segHeadLine(lines, seg), ...body]]]))
}

// depends_on 移除；from 段不存在或无该依赖返回原文
export function removeDependency(text, from, to) {
  const t = String(text || '')
  const { lines, preamble, segs } = splitSegments(t)
  const seg = segs.find((s) => s.name === from)
  if (!seg) return t
  const body = segBody(lines, seg)
  const depIdx = body.findIndex((l) => /^ {4}depends_on:/.test(l))
  if (depIdx < 0) return t
  const depLine = body[depIdx]
  const flow = depLine.match(/^ {4}depends_on:\s*\[(.*)\]\s*$/)
  if (flow) {
    const list = flow[1].split(',').map((x) => x.trim().replace(/^['"]|['"]$/g, '')).filter(Boolean)
    if (!list.includes(to)) return t
    const next = list.filter((x) => x !== to)
    if (next.length) body[depIdx] = `    depends_on: [${next.join(', ')}]`
    else body.splice(depIdx, 1)
    return joinSegments(lines, preamble, segs, new Map([[from, [segHeadLine(lines, seg), ...body]]]))
  }
  const nextLine = body[depIdx + 1] || ''
  const isObjectForm = /^ {6}[A-Za-z0-9_.-]+:/.test(nextLine) && !/^ {6}-/.test(nextLine)
  if (isObjectForm) return rewriteServiceDep(text, from, to, 'remove')
  const target = body.findIndex((l) => depListLineRe(to).test(l))
  if (target < 0) return t
  body.splice(target, 1)
  // 列表删空后连同 depends_on: 行一起删
  const after = body[depIdx + 1] || ''
  if (!/^ {6,}\S/.test(after)) body.splice(depIdx, 1)
  return joinSegments(lines, preamble, segs, new Map([[from, [segHeadLine(lines, seg), ...body]]]))
}

// 对象形态（condition）依赖增删：整服务重排（保守路径，预设不用该形态）
function rewriteServiceDep(text, from, to, op) {
  const model = parseBlock(text)
  if (!model || !model[from]) return null
  const body = model[from]
  const deps = Array.isArray(body.depends_on) ? [...body.depends_on] : body.depends_on && typeof body.depends_on === 'object'
    ? Object.keys(body.depends_on) : body.depends_on ? [body.depends_on] : []
  if (op === 'add') {
    if (!deps.includes(to)) deps.push(to)
  } else {
    const idx = deps.indexOf(to)
    if (idx >= 0) deps.splice(idx, 1)
  }
  if (deps.length) body.depends_on = deps
  else delete body.depends_on
  return updateService(text, from, body)
}

// 属性面板编辑：整服务规范化重排（只重排该服务段，其余段文本原样保留）
export function updateService(text, name, body) {
  const { lines, preamble, segs } = splitSegments(String(text || ''))
  const seg = segs.find((s) => s.name === name)
  if (!seg) return null
  return joinSegments(lines, preamble, segs, new Map([[name, dumpSegment(name, body).split('\n')]]))
}

// 追加服务（末尾新段）
export function addService(text, name, body) {
  const t = String(text || '')
  const seg = dumpSegment(name, body || { image: '', restart: 'unless-stopped' })
  const prefix = t && !t.endsWith('\n') ? '\n' : ''
  return t + prefix + seg
}

// 删除服务（级联清理各段 depends_on 引用）
export function removeService(text, name) {
  let t = String(text || '')
  const { lines, preamble, segs } = splitSegments(t)
  const seg = segs.find((s) => s.name === name)
  if (!seg) return t
  const overrides = new Map()
  overrides.set(name, [])
  for (const other of segs) {
    if (other.name === name) continue
    const body = segBody(lines, other)
    const kept = []
    let depIdx = -1
    let removed = false
    body.forEach((l, i) => {
      if (/^ {4}depends_on:/.test(l)) depIdx = kept.length
      if (depListLineRe(name).test(l)) {
        removed = true
        return
      }
      kept.push(l)
    })
    if (removed) {
      // 列表删空则连 depends_on 行删
      const after = kept[depIdx + 1] || ''
      if (depIdx >= 0 && !/^ {6,}\S/.test(after)) kept.splice(depIdx, 1)
      overrides.set(other.name, [segHeadLine(lines, other), ...kept])
    }
  }
  const next = joinSegments(lines, preamble, segs, overrides)
  // 清理多余空行（删除段留下的尾部空行）并保证非空文本以单个换行结尾
  const cleaned = next.replace(/\n{3,}/g, '\n\n').replace(/\s+$/, '')
  return cleaned ? cleaned + '\n' : ''
}

// 服务改名（段头 + 各段 depends_on 列表项引用同步）
export function renameService(text, oldName, newName) {
  const t = String(text || '')
  const { lines, preamble, segs } = splitSegments(t)
  const seg = segs.find((s) => s.name === oldName)
  if (!seg) return null
  const overrides = new Map()
  overrides.set(oldName, [segHeadLine(lines, seg).replace(SEG_HEAD_RE, `  ${newName}:`), ...segBody(lines, seg)])
  for (const other of segs) {
    if (other.name === oldName) continue
    const body = segBody(lines, other).map((l) => (depListLineRe(oldName).test(l) ? l.replace(`- ${oldName}`, `- ${newName}`) : l))
    if (body.some((l) => depListLineRe(newName).test(l))) {
      overrides.set(other.name, [segHeadLine(lines, other), ...body])
    }
  }
  return joinSegments(lines, preamble, segs, overrides)
}

// YAML 解析错误信息（首行）；用于图形模式禁用时的提示
export function blockError(text) {
  try {
    yaml.load(protectStr('services:\n' + String(text || '')))
    return ''
  } catch (e) {
    const msg = (e && e.message) || String(e)
    return msg.split('\n')[0]
  }
}
