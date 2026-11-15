// 编排服务块 YAML 上下文补全（CodeMirror 6 CompletionSource）
// 按行前缀缩进判定补全场景（编排块结构规整：2=服务名 / 4=服务体键 / 6=列表项或嵌套键）：
//   服务体键位（含 healthcheck 等整块模板）/ depends_on 目标（块内 + 外部服务，标注来源）/
//   image 镜像地址 / {{变量}}（含未闭合智能补 }}）/ ${.env 变量}
// 所有数据经 getter 注入保持响应式：{ services, extraServices, images, vars, envVars }
export function composeCompletion(opts) {
  const get = (k, dflt) => (typeof opts[k] === 'function' ? opts[k]() || dflt : opts[k] || dflt)
  return (ctx) => {
    const line = ctx.state.doc.lineAt(ctx.pos)
    const before = line.text.slice(0, ctx.pos - line.from)

    // {{变量}}（未闭合时光标后无 }} 则智能补全闭合）
    let m = before.match(/\{\{(\w*)$/)
    if (m) {
      const vars = get('vars', [])
      if (!vars.length) return null
      return {
        from: ctx.pos - m[1].length,
        options: vars.map((v) => ({
          label: v.name,
          detail: [v.label, v.origin].filter(Boolean).join(' · '),
          type: 'variable',
          apply(view, c, from, to) {
            const after = view.state.sliceDoc(to, to + 2)
            view.dispatch({ changes: { from, to, insert: after === '}}' ? c.label : c.label + '}}' } })
          }
        })),
        validFor: /^\w*$/
      }
    }

    // ${.env 变量（凭据/组件 env，closeBrackets 已闭合单个 }）
    m = before.match(/\$\{(\w*)$/)
    if (m) {
      const envs = get('envVars', [])
      if (!envs.length) return null
      return {
        from: ctx.pos - m[1].length,
        options: envs.map((e) => (typeof e === 'string' ? { label: e, type: 'variable' } : { label: e.name, detail: e.origin, type: 'variable' })),
        validFor: /^\w*$/
      }
    }

    // 当前光标所属的 4 空格键（向上回溯）
    const currentKey = (() => {
      const text = ctx.state.doc.toString()
      const lines = text.split('\n')
      const lineNo = line.number - 1
      for (let i = lineNo; i >= 0; i--) {
        const km = lines[i].match(/^ {4}([A-Za-z0-9_.-]+):/)
        if (km) return km[1]
      }
      return null
    })()

    // depends_on 块列表项：目标服务名
    if (/^ {6}-\s*\S*$/.test(before) && currentKey === 'depends_on') {
      return serviceOptions(ctx, before.match(/^ {6}-\s*(\S*)$/)[1])
    }
    // depends_on flow 列表元素
    m = before.match(/^ {4}depends_on:\s*\[([^\]]*)$/)
    if (m) {
      const typed = m[1].split(',').pop().trim().replace(/^['"]/, '')
      return serviceOptions(ctx, typed)
    }
    // image 值
    m = before.match(/^ {4}image:\s*(\S*)$/)
    if (m) {
      const images = [...new Set(get('images', []))].filter(Boolean)
      if (!images.length) return null
      return { from: ctx.pos - m[1].length, options: images.map((i) => ({ label: i, type: 'constant' })), validFor: /^\S*$/ }
    }
    // 服务体键位（4 空格键位）
    m = before.match(/^ {4}(\w*)$/)
    if (m) {
      return { from: ctx.pos - m[1].length, options: keyOptions(), validFor: /^[\w-]*$/ }
    }
    // environment 嵌套键（6 空格映射键）
    m = before.match(/^ {6}(\w*)$/)
    if (m && currentKey === 'environment') {
      const opts2 = [
        { label: 'TZ', detail: '时区', type: 'property', apply: ins('TZ: Asia/Shanghai') },
        ...get('envVars', []).map((e) => ({
          label: typeof e === 'string' ? e : e.name,
          detail: typeof e === 'string' ? '' : e.origin,
          type: 'property',
          apply: ins(`${typeof e === 'string' ? e : e.name}: ${'${'}${typeof e === 'string' ? e : e.name}${'}'}`)
        }))
      ]
      return { from: ctx.pos - m[1].length, options: opts2, validFor: /^[\w-]*$/ }
    }
    return null

    // depends_on 目标：块内服务 + 外部服务（标注来源）
    function serviceOptions(c, typed) {
      const names = new Set(get('services', []))
      const options = []
      for (const s of get('services', [])) {
        options.push({ label: s, detail: '本块服务', type: 'class' })
      }
      for (const e of get('extraServices', [])) {
        if (names.has(e.name)) continue
        options.push({ label: e.name, detail: `外部 · ${e.origin || ''}`, type: 'interface' })
      }
      if (!options.length) return null
      return { from: c.pos - typed.length, options, validFor: /^[\w.-]*$/ }
    }
  }

  function ins(text) {
    return (view, c, from, to) => view.dispatch({ changes: { from, to, insert: text } })
  }

  // 服务体键位清单（常用 compose 键 + 整块模板）
  function keyOptions() {
    const o = (label, detail) => ({ label, detail, type: 'property', apply: ins(`${label}: `) })
    const block = (label, detail, body) => ({ label, detail, type: 'property', apply: ins(`${label}:\n${body}`) })
    return [
      o('image', '镜像地址'),
      block('ports', '端口映射', '      - "8080:8080"'),
      block('environment', '环境变量（映射或 K=V 列表）', '      TZ: Asia/Shanghai'),
      block('depends_on', '启动依赖（服务名列表）', '      - '),
      block('volumes', '数据卷（建议 bind 到 ./data/<名>）', '      - ./data/<名>:/容器路径'),
      o('command', '启动命令'),
      block('healthcheck', '健康检查', '      test: ["CMD", "curl", "-f", "http://localhost:8080"]\n      interval: 30s\n      timeout: 5s\n      retries: 5'),
      { label: 'restart', detail: '重启策略', type: 'property', apply: ins('restart: unless-stopped') },
      o('container_name', '容器名'),
      o('user', '运行用户'),
      { label: 'init', detail: '进程 1 号 init', type: 'property', apply: ins('init: true') },
      block('networks', '网络', '      - default'),
      block('sysctls', '内核参数', '      net.core.somaxconn: 1024'),
      o('entrypoint', '入口命令')
    ]
  }
}
