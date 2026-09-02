<script setup>
import { ref, computed, watch, nextTick, onMounted, onBeforeUnmount } from 'vue'
import {
  GetMousePos,
  RunSteps,
  IsRunning,
  StartRecord,
  StopRecord,
  RecorderState,
  SaveSteps,
  LoadSteps,
  OpenLogFolder,
  ListWindowTitles,
} from '../wailsjs/go/main/App.js'
import { EventsOn, EventsOff } from '../wailsjs/runtime/runtime.js'

// ===== 步骤类型定义 =====
const STEP_TYPES = [
  { value: 'move',        label: '移动鼠标',  needXY: true,  needButton: false, needKey: false, needText: false, needTitle: false },
  { value: 'click',       label: '单击',     needXY: true,  needButton: true,  needKey: false, needText: false, needTitle: false },
  { value: 'doubleclick', label: '双击',     needXY: true,  needButton: true,  needKey: false, needText: false, needTitle: false },
  { value: 'keypress',    label: '按键(按下+释放)', needXY: false, needButton: false, needKey: true,  needText: false, needTitle: false },
  { value: 'keydown',     label: '按下按键',  needXY: false, needButton: false, needKey: true,  needText: false, needTitle: false },
  { value: 'keyup',       label: '释放按键',  needXY: false, needButton: false, needKey: true,  needText: false, needTitle: false },
  { value: 'type',        label: '输入文本',  needXY: false, needButton: false, needKey: false, needText: true,  needTitle: false },
  { value: 'movewindow',  label: '移动窗口',  needXY: true,  needButton: false, needKey: false, needText: false, needTitle: true  },
]

// ===== 状态 =====
const steps = ref([])
const draft = ref({
  type: 'click',
  x: 0, y: 0,
  button: 'left',
  key: '',
  text: '',
  title: '',
  delayMs: 0,
})
const loopCount = ref(1)
const intervalMs = ref(1000)
const startDelayMs = ref(3000)
const defaultStepDelay = ref(1000)  // 录制步骤 / 添加步骤的默认 DelayMs
const recordAppend = ref(true) // 录制完成时是追加还是替换
// 仅首次执行的操作类型:这些类型只在第 1 次循环执行,后续循环跳过。默认包含 movewindow。
const onceTypes = ref(['movewindow'])

const isRunning = ref(false)
const progress = ref({ loop: 0, totalLoop: 0, step: 0, totalStep: 0 })
const nextId = ref(1)

const recordState = ref('idle') // idle | armed | recording

// 是否实时显示鼠标坐标(设置面板可关,关闭后停止轮询省 IPC)
const showMousePos = ref(true)

// 当前鼠标屏幕坐标(轮询 Go 获取,鼠标在窗口外也实时)
const mousePos = ref({ x: 0, y: 0 })
let mousePosTimer = null

// 当前桌面所有可见顶层窗口的标题(去重),供"移动窗口"步骤的下拉选择器使用。
const windowTitles = ref([])
async function refreshWindowTitles() {
  try {
    const list = await ListWindowTitles()
    windowTitles.value = Array.isArray(list) ? list : []
  } catch (e) {
    windowTitles.value = []
    pushLog(`✗ 枚举窗口失败: ${e}`)
  }
}
// 把"当前标题值"也合进候选列表(编辑已存在步骤时,该窗口可能已不在桌面上)。
function windowTitleOptions(extra) {
  const set = new Set(windowTitles.value)
  if (extra && !set.has(extra)) {
    return [...windowTitles.value, extra]
  }
  return windowTitles.value
}

function startMousePosWatch() {
  stopMousePosWatch()
  const tick = async () => {
    try {
      mousePos.value = await GetMousePos()
    } catch (e) { /* 忽略瞬时失败 */ }
  }
  tick() // 立即取一次,避免初始显示 0,0
  mousePosTimer = setInterval(tick, 250)
}

function stopMousePosWatch() {
  if (mousePosTimer) {
    clearInterval(mousePosTimer)
    mousePosTimer = null
  }
}

// 开关联动:开启才轮询,关闭立即停
watch(showMousePos, (on) => {
  if (on) startMousePosWatch()
  else stopMousePosWatch()
})

const currentTypeMeta = computed(
  () => STEP_TYPES.find(t => t.value === draft.value.type) || STEP_TYPES[0]
)

// 添加面板里切到「移动窗口」类型时,坐标自动预填默认值 (100, 100) 并刷新窗口列表
watch(() => draft.value.type, (t) => {
  if (t === 'movewindow') {
    draft.value.x = 100
    draft.value.y = 100
    refreshWindowTitles()
  }
})
// 编辑面板切到「移动窗口」时也刷新窗口列表
watch(() => editDraft.value.type, (t) => {
  if (t === 'movewindow') {
    refreshWindowTitles()
  }
})

const recordBtnLabel = computed(() => {
  if (recordState.value === 'recording') return '⏹ 结束录制【F8】'
  if (recordState.value === 'armed') return '🟡 F8待命'
  return '⏺ 开始录制【F8】'
})
const recordBtnTitle = computed(() => {
  if (recordState.value === 'recording') return '正在录制 · 按 F8 结束'
  if (recordState.value === 'armed') return '已就绪 · 切到目标窗口后按 F8 开始录制'
  return '点此或按 F8 启动录制钩子(再按 F8 结束)'
})
const recordBtnClass = computed(() => `record-btn ${recordState.value}`)

// "开始执行" 按钮旁边的小字参数摘要(两行)
const runSummaryLines = computed(() => {
  const lc = loopCount.value
  const iv = intervalMs.value
  const sd = startDelayMs.value
  return [`循环 ${lc} 次 · 间隔 ${iv}ms`, `启动延迟 ${sd}ms`]
})

// 打开日志目录
async function openLog() {
  try {
    await OpenLogFolder()
  } catch (e) {
    console.error('打开日志目录失败:', e)
  }
}

// ===== 设置弹层 =====
const settingsOpen = ref(false)

// ===== 步骤编辑 =====
const editingId = ref(null)
const editDraft = ref({})

// ===== 步骤插入(行内添加面板) =====
// addingAtIdx: null = 面板关闭;数值 = 插入到该位置(0 表示插到最前面;N 表示插到第 N 个位置 = 当前 N-1 步之后)
const addingAtIdx = ref(null)

// ===== 脚本存读 =====
async function saveScript() {
  if (steps.value.length === 0) {
    pushLog('✗ 步骤列表为空,无法保存')
    return
  }
  try {
    const path = await SaveSteps(steps.value)
    if (!path) {
      pushLog('已取消保存')
      return
    }
    pushLog(`✔ 已保存 ${steps.value.length} 步到 ${path}`)
  } catch (e) {
    pushLog(`✗ 保存失败: ${e}`)
  }
}

async function loadScript() {
  try {
    const loaded = await LoadSteps()
    if (!loaded || loaded.length === 0) {
      pushLog('已取消导入(或文件为空)')
      return
    }
    if (steps.value.length > 0) {
      const ok = confirm(`当前有 ${steps.value.length} 步,导入的 ${loaded.length} 步要追加还是替换?\n\n确定 = 追加,取消 = 替换`)
      if (!ok) steps.value = []
    }
    for (const s of loaded) {
      steps.value.push({
        id: nextId.value++,
        type: s.type || 'click',
        x: s.x || 0,
        y: s.y || 0,
        button: s.button || 'left',
        key: s.key || '',
        text: s.text || '',
        title: s.title || '',
        delayMs: s.delayMs || 0,
      })
    }
    pushLog(`✔ 已导入 ${loaded.length} 步,当前共 ${steps.value.length} 步`)
  } catch (e) {
    pushLog(`✗ 导入失败: ${e}`)
  }
}

// ===== 日志 =====
// UI 不再显示日志(日志写入文件,在设置弹层/日志按钮里能打开目录)。
// 保留 pushLog 桩,所有调用点不用改;需要调试可在 console 看到。
function pushLog(line) {
  // 日志已由 Go 端写入 %APPDATA%/mousemacro/log.txt
  if (typeof console !== 'undefined') console.log('[mousemacro]', line)
}

// ===== 步骤操作 =====
function addStep() {
  const d = draft.value
  const meta = currentTypeMeta.value
  if (meta.needXY && (d.x === '' || d.x == null || d.y === '' || d.y == null)) {
    pushLog('✗ 请填写 X / Y 坐标')
    return
  }
  if (meta.needKey && !d.key.trim()) {
    pushLog('✗ 请填写按键表达式')
    return
  }
  if (meta.needText && !d.text) {
    pushLog('✗ 请填写要输入的文本')
    return
  }
  if (meta.needTitle && !d.title.trim()) {
    pushLog('✗ 请填写窗口标题关键字')
    return
  }
  const step = {
    id: nextId.value++,
    type: d.type,
    x: Number(d.x) || 0,
    y: Number(d.y) || 0,
    button: d.button || 'left',
    key: d.key || '',
    text: d.text || '',
    title: d.title || '',
    // 用户没填延迟时,用"默认延迟"作为兜底
    delayMs: Number(d.delayMs) || Number(defaultStepDelay.value) || 0,
  }
  steps.value.push(step)
  pushLog(`+ 添加步骤 #${steps.value.length}: ${formatStep(step)}`)
}

function deleteStep(idx) {
  const removed = steps.value.splice(idx, 1)[0]
  pushLog(`- 删除步骤: ${formatStep(removed)}`)
}

function moveUp(idx) {
  if (idx <= 0) return
  const t = steps.value[idx]
  steps.value.splice(idx, 1)
  steps.value.splice(idx - 1, 0, t)
}

function moveDown(idx) {
  if (idx >= steps.value.length - 1) return
  const t = steps.value[idx]
  steps.value.splice(idx, 1)
  steps.value.splice(idx + 1, 0, t)
}

function startEdit(idx) {
  const s = steps.value[idx]
  editingId.value = s.id
  // 深拷贝当前步骤到 editDraft
  editDraft.value = {
    type: s.type,
    x: s.x,
    y: s.y,
    button: s.button,
    key: s.key,
    text: s.text,
    title: s.title,
    delayMs: s.delayMs,
  }
}

function saveEdit(idx) {
  const s = steps.value[idx]
  const ed = editDraft.value
  s.type = ed.type
  s.x = Number(ed.x) || 0
  s.y = Number(ed.y) || 0
  s.button = ed.button || 'left'
  s.key = ed.key || ''
  s.text = ed.text || ''
  s.title = ed.title || ''
  s.delayMs = Number(ed.delayMs) || 0
  editingId.value = null
  pushLog(`✓ 已编辑步骤 #${idx + 1}: ${formatStep(s)}`)
}

function cancelEdit() {
  editingId.value = null
}

function startAddAt(idx) {
  // 打开"插入到 idx 位置"的添加面板;同时重置 draft 到默认值,确保每次打开都是干净的
  draft.value = {
    type: 'click',
    x: 0, y: 0,
    button: 'left',
    key: '',
    text: '',
    title: '',
    delayMs: Number(defaultStepDelay.value) || 0,
  }
  editingId.value = null // 关闭任何正在编辑的步骤
  addingAtIdx.value = idx
}

function submitAdd() {
  if (addingAtIdx.value == null) return
  const d = draft.value
  const meta = currentTypeMeta.value
  if (meta.needXY && (d.x === '' || d.x == null || d.y === '' || d.y == null)) {
    pushLog('✗ 请填写 X / Y 坐标')
    return
  }
  if (meta.needKey && !d.key.trim()) {
    pushLog('✗ 请填写按键表达式')
    return
  }
  if (meta.needText && !d.text) {
    pushLog('✗ 请填写要输入的文本')
    return
  }
  if (meta.needTitle && !d.title.trim()) {
    pushLog('✗ 请填写窗口标题关键字')
    return
  }
  const step = {
    id: nextId.value++,
    type: d.type,
    x: Number(d.x) || 0,
    y: Number(d.y) || 0,
    button: d.button || 'left',
    key: d.key || '',
    text: d.text || '',
    title: d.title || '',
    delayMs: Number(d.delayMs) || Number(defaultStepDelay.value) || 0,
  }
  const insertPos = Math.max(0, Math.min(addingAtIdx.value, steps.value.length))
  steps.value.splice(insertPos, 0, step)
  const where = insertPos === 0 ? '最前面'
    : insertPos >= steps.value.length ? '末尾'
    : `第 ${insertPos} 位(即原第 ${insertPos} 步之前)`
  pushLog(`+ 在${where}插入: ${formatStep(step)}`)
  addingAtIdx.value = null
}

function cancelAdd() {
  addingAtIdx.value = null
}

const editTypeMeta = computed(() => {
  if (editingId.value == null) return null
  return STEP_TYPES.find(t => t.value === editDraft.value.type) || STEP_TYPES[0]
})

function clearSteps() {
  if (steps.value.length === 0) return
  if (!confirm(`确认清空全部 ${steps.value.length} 个步骤?`)) return
  steps.value = []
  pushLog('已清空步骤列表')
}

function formatStep(s) {
  switch (s.type) {
    case 'move':        return `移动 → (${s.x}, ${s.y})`
    case 'click':       return `${s.button === 'right' ? '右键' : '左键'}单击 @ (${s.x}, ${s.y})`
    case 'doubleclick': return `${s.button === 'right' ? '右键' : '左键'}双击 @ (${s.x}, ${s.y})`
    case 'keypress':    return `按键 ${s.key}`
    case 'keydown':     return `按下 ${s.key}`
    case 'keyup':       return `释放 ${s.key}`
    case 'type':        return `输入文本 "${s.text}"`
    case 'movewindow':  return `移动窗口 "${s.title}" → (${s.x}, ${s.y})`
    default:            return JSON.stringify(s)
  }
}

const TYPE_BADGE = {
  move: '移动', click: '单击', doubleclick: '双击',
  keypress: '按键', keydown: '按下', keyup: '释放', type: '输入', movewindow: '窗口',
}
function typeBadge(t) { return TYPE_BADGE[t] || t }

// ===== 坐标 =====
async function captureCurrentPos() {
  try {
    const pos = await GetMousePos()
    draft.value.x = pos.x
    draft.value.y = pos.y
    pushLog(`⤵ 已捕获当前坐标 (${pos.x}, ${pos.y})`)
  } catch (e) {
    pushLog(`✗ 取坐标失败: ${e}`)
  }
}

// ===== 执行控制 =====
async function startRun() {
  if (steps.value.length === 0) {
    pushLog('✗ 步骤列表为空,无法执行')
    return
  }
  if (loopCount.value < 1) {
    pushLog('✗ 循环次数必须 >= 1')
    return
  }
  try {
    pushLog(`▶ 启动: ${steps.value.length} 步 × ${loopCount.value} 次,启动延迟 ${startDelayMs.value} ms,循环间隔 ${intervalMs.value} ms`)
    progress.value = { loop: 0, totalLoop: loopCount.value, step: 0, totalStep: steps.value.length }
    isRunning.value = true
    await RunSteps(
      steps.value,
      Number(loopCount.value),
      Number(intervalMs.value),
      Number(startDelayMs.value),
      onceTypes.value,
    )
  } catch (e) {
    isRunning.value = false
    pushLog(`✗ 启动失败: ${e}`)
  }
}

async function stopRun() {
  // 已停用 — 停止由 F12 全局热键触发
}

const progressPct = computed(() => {
  const { loop, totalLoop, step, totalStep } = progress.value
  if (!totalLoop || !totalStep) return 0
  const perLoop = 100 / totalLoop
  return Math.min(100, ((loop - 1) * perLoop) + (step / totalStep) * perLoop)
})

// 当前正在执行的步骤索引(0-based);未运行或没到具体步骤时为 -1。
// 用于步骤列表里给正在执行的步骤加红点标识。
const currentStepIdx = computed(() => {
  if (!isRunning.value || progress.value.step < 1) return -1
  return progress.value.step - 1
})

// 当前执行步骤变化时,滚动步骤列表让红点可见。
// block:'nearest' 只在红点滚出视口时才滚动,尽量不打扰用户手动滚动。
watch(currentStepIdx, (idx) => {
  if (idx < 0) return
  nextTick(() => {
    const el = document.querySelectorAll('.step-item')[idx]
    if (el) el.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
  })
})

// ===== 录制 =====
async function toggleRecord() {
  if (recordState.value === 'idle') {
    try {
      await StartRecord()
      pushLog('⏺ 录制器已启动,等待 F8')
    } catch (e) {
      pushLog(`✗ 启动录制失败: ${e}`)
    }
  } else {
    // armed 或 recording 都允许强制停止
    try {
      await StopRecord()
      pushLog('⏹ 已发送停止录制信号')
    } catch (e) {
      pushLog(`✗ 停止录制失败: ${e}`)
    }
  }
}

// 把 Go 推过来的 Step[] 接入到前端列表
function ingestRecordedSteps(newSteps) {
  if (!newSteps || newSteps.length === 0) {
    pushLog('✎ 录制结果为空,未添加步骤')
    return
  }
  if (!recordAppend.value) {
    steps.value = []
  }
  // 录制的步骤一律用前端配置的"默认延迟",覆盖 Go 端的 hardcode 值
  const recordedDelay = Number(defaultStepDelay.value) || 0
  for (const s of newSteps) {
    steps.value.push({
      id: nextId.value++,
      type: s.type,
      x: s.x || 0,
      y: s.y || 0,
      button: s.button || 'left',
      key: s.key || '',
      text: s.text || '',
      delayMs: recordedDelay,
    })
  }
  pushLog(`+ 录制完成,已${recordAppend.value ? '追加' : '替换'} ${newSteps.length} 步`)
}

// ===== 设置持久化(localStorage) =====
// 循环/间隔/延迟/录制追加/仅首次执行列表 刷新后保留。
const SETTINGS_KEY = 'mouseassistant.settings.v1'

function loadSettings() {
  try {
    const raw = localStorage.getItem(SETTINGS_KEY)
    if (!raw) return
    const s = JSON.parse(raw)
    if (typeof s.loopCount === 'number' && s.loopCount >= 1) loopCount.value = s.loopCount
    if (typeof s.intervalMs === 'number') intervalMs.value = s.intervalMs
    if (typeof s.startDelayMs === 'number') startDelayMs.value = s.startDelayMs
    if (typeof s.defaultStepDelay === 'number') defaultStepDelay.value = s.defaultStepDelay
    if (typeof s.recordAppend === 'boolean') recordAppend.value = s.recordAppend
    if (typeof s.showMousePos === 'boolean') showMousePos.value = s.showMousePos
    if (Array.isArray(s.onceTypes)) onceTypes.value = s.onceTypes.filter(t => STEP_TYPES.some(x => x.value === t))
  } catch (e) { /* 忽略损坏的存储数据 */ }
}

function saveSettings() {
  const s = {
    loopCount: loopCount.value,
    intervalMs: intervalMs.value,
    startDelayMs: startDelayMs.value,
    defaultStepDelay: defaultStepDelay.value,
    recordAppend: recordAppend.value,
    showMousePos: showMousePos.value,
    onceTypes: onceTypes.value,
  }
  try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(s)) } catch (e) { /* 存储满/禁用时忽略 */ }
}

// 任一设置变化即持久化(deep: onceTypes 是数组内部增删)
watch(
  [loopCount, intervalMs, startDelayMs, defaultStepDelay, recordAppend, showMousePos, onceTypes],
  saveSettings,
  { deep: true },
)

// ===== 事件订阅 =====
function bindEvents() {
  EventsOn('run:started', (info) => {
    isRunning.value = true
    progress.value = { loop: 0, totalLoop: info.loopCount, step: 0, totalStep: info.steps }
  })
  EventsOn('run:progress', (p) => {
    progress.value = p
  })
  EventsOn('run:completed', () => {
    isRunning.value = false
    pushLog('✅ 全部完成')
  })
  EventsOn('run:stopped', (info) => {
    isRunning.value = false
    if (info?.reason === 'aborted') {
      pushLog(`⏹ 已中止 (${info.when || ''}${info.loop ? ' @ ' + info.loop + '次' : ''}${info.step ? ' / ' + info.step + '步' : ''})`)
    } else if (info?.reason === 'error') {
      pushLog(`✗ 错误 (第 ${info.loop} 次 / 第 ${info.step} 步): ${info.err}`)
    }
  })

  EventsOn('record:state', (state) => {
    recordState.value = state || 'idle'
  })
  EventsOn('record:complete', (newSteps) => {
    ingestRecordedSteps(newSteps)
  })
}

function unbindEvents() {
  EventsOff('run:started')
  EventsOff('run:progress')
  EventsOff('run:completed')
  EventsOff('run:stopped')
  EventsOff('record:state')
  EventsOff('record:complete')
}

onMounted(async () => {
  loadSettings()
  bindEvents()
  if (showMousePos.value) startMousePosWatch()
  try {
    isRunning.value = await IsRunning()
    recordState.value = await RecorderState()
  } catch (e) { /* ignore */ }
})

onBeforeUnmount(() => {
  unbindEvents()
  stopMousePosWatch()
})
</script>

<template>
  <div class="app">
    <header class="topbar">
      <!-- 执行进度条:运行时显示在状态文本左侧 -->
      <div class="progress top-progress" v-if="isRunning">
        <div class="bar" :style="{ width: progressPct + '%' }"></div>
      </div>
      <div class="status" :class="{ running: isRunning }">
        <span class="dot"></span>
        {{ isRunning ? `运行中: ${progress.loop}/${progress.totalLoop} 次 · ${progress.step}/${progress.totalStep} 步` : '就绪' }}
      </div>
    </header>

    <div class="recordbar">
      <button :class="recordBtnClass" @click="toggleRecord" :disabled="isRunning" :title="recordBtnTitle">
        {{ recordBtnLabel }}
      </button>
      <button class="run-inline" @click="startRun" :disabled="isRunning || steps.length === 0" title="开始执行宏脚本(也可按 F12)">
        ▶ 开始执行【F12】
      </button>
      <span class="run-summary">
        <span v-for="line in runSummaryLines" :key="line">{{ line }}</span>
      </span>

      <span class="mouse-pos" v-if="showMousePos" title="当前鼠标屏幕坐标">
        <span>x:{{ mousePos.x }}</span>
        <span>y:{{ mousePos.y }}</span>
      </span>

      <button class="ghost-inline" @click="settingsOpen = !settingsOpen" :class="{ active: settingsOpen }" title="执行参数">
        ⚙ 设置
      </button>

      <!-- 设置弹层 -->
      <div v-if="settingsOpen" class="settings-popover" @click.stop>
        <div class="settings-title">⚙ 执行参数</div>
        <div class="settings-grid">
          <div class="field">
            <label>循环次数</label>
            <input type="number" v-model.number="loopCount" min="1" />
          </div>
          <div class="field">
            <label>循环间隔 (ms)</label>
            <input type="number" v-model.number="intervalMs" min="0" step="100" />
          </div>
          <div class="field">
            <label>默认延迟 (ms)</label>
            <input type="number" v-model.number="defaultStepDelay" min="0" step="100" />
          </div>
          <div class="field">
            <label>启动延迟 (ms)</label>
            <input type="number" v-model.number="startDelayMs" min="0" step="500" />
            <small class="hint">给切窗口留时间,期间按 Esc 取消</small>
          </div>
          <div class="field record-append-field" v-if="recordState === 'idle'">
            <label class="record-append">
              <input type="checkbox" v-model="recordAppend" /> 录制后追加(否则替换现有步骤)
            </label>
          </div>
          <div class="field record-append-field">
            <label class="record-append">
              <input type="checkbox" v-model="showMousePos" /> 实时显示鼠标坐标
            </label>
          </div>
          <div class="field once-types-field">
            <label>仅首次执行的操作</label>
            <div class="once-types">
              <label v-for="t in STEP_TYPES" :key="t.value" :class="{ checked: onceTypes.includes(t.value) }">
                <input type="checkbox" :value="t.value" v-model="onceTypes" /> {{ t.label }}
              </label>
            </div>
            <small class="hint">勾选后:该类型步骤只在第 1 次循环执行,后续循环直接跳过</small>
          </div>
        </div>
        <div class="settings-foot">
          <button class="ghost" @click="openLog" title="打开日志所在文件夹">📁 打开日志文件夹</button>
          <button class="ghost" @click="settingsOpen = false">关闭</button>
        </div>
      </div>
      <div v-if="settingsOpen" class="settings-backdrop" @click="settingsOpen = false"></div>
    </div>

    <div class="content">
      <div class="right-column">
        <section class="panel step-panel">
          <div class="panel-head">
            <h2>步骤列表 ({{ steps.length }})</h2>
            <div class="head-actions">
              <button class="ghost small" @click="startAddAt(0)" :disabled="isRunning || addingAtIdx !== null" title="在列表最前面插入一个步骤">➕ 添加</button>
              <button class="ghost small" @click="saveScript" :disabled="isRunning || steps.length === 0" title="保存到 JSON 文件">💾 保存</button>
              <button class="ghost small" @click="loadScript" :disabled="isRunning" title="从 JSON 文件导入">📂 导入</button>
              <button class="ghost small" @click="clearSteps" :disabled="isRunning || steps.length === 0" title="清空列表">清空</button>
            </div>
          </div>

          <div class="step-list" v-if="steps.length > 0 || addingAtIdx !== null">
            <!-- 在最前面插入的添加面板(addingAtIdx === 0) -->
            <div v-if="addingAtIdx === 0" class="step-add-panel">
              <div class="add-panel-title">↓ 插入到最前面(将成为新的第 1 步)</div>
              <div class="add-form">
                <div class="add-row">
                  <label>类型</label>
                  <select v-model="draft.type">
                    <option v-for="t in STEP_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
                  </select>
                </div>
                <template v-if="currentTypeMeta.needXY">
                  <div class="add-row">
                    <label>X / Y</label>
                    <div class="xy">
                      <input type="number" v-model.number="draft.x" placeholder="X" />
                      <input type="number" v-model.number="draft.y" placeholder="Y" />
                      <button type="button" class="ghost" @click="captureCurrentPos" :disabled="isRunning">⤵ 取当前位置</button>
                    </div>
                  </div>
                </template>
                <template v-if="currentTypeMeta.needButton">
                  <div class="add-row">
                    <label>按键</label>
                    <div class="seg">
                      <label><input type="radio" v-model="draft.button" value="left" /> 左键</label>
                      <label><input type="radio" v-model="draft.button" value="right" /> 右键</label>
                    </div>
                  </div>
                </template>
                <template v-if="currentTypeMeta.needKey">
                  <div class="add-row">
                    <label>按键</label>
                    <input type="text" v-model="draft.key" placeholder='如 a / Enter / ctrl+s / shift+F5' />
                  </div>
                </template>
                <template v-if="currentTypeMeta.needText">
                  <div class="add-row">
                    <label>文本</label>
                    <input type="text" v-model="draft.text" placeholder="要逐字符输入的文本" />
                  </div>
                </template>
                <template v-if="currentTypeMeta.needTitle">
                  <div class="add-row">
                    <label>窗口标题</label>
                    <div class="title-row">
                      <select v-model="draft.title" class="grow">
                        <option value="" disabled>-- 选择窗口 --</option>
                        <option v-for="t in windowTitles" :key="t" :value="t">{{ t }}</option>
                      </select>
                      <button type="button" class="ghost" @click="refreshWindowTitles" :disabled="isRunning" title="刷新窗口列表">↻</button>
                    </div>
                    <small class="hint">共 {{ windowTitles.length }} 个可见窗口,下拉选择即作为匹配关键字</small>
                  </div>
                </template>
                <div class="add-row">
                  <label>延迟 (ms)</label>
                  <input type="number" v-model.number="draft.delayMs" :placeholder="String(defaultStepDelay)" />
                </div>
              </div>
              <div class="add-actions">
                <button class="primary" @click="submitAdd">✓ 添加</button>
                <button class="ghost" @click="cancelAdd">× 取消</button>
              </div>
            </div>

            <!-- 步骤列表 + 步骤后的添加面板 -->
            <template v-for="(s, idx) in steps" :key="s.id">
              <div :class="['step-item', { editing: editingId === s.id, active: currentStepIdx === idx }]">
                <template v-if="editingId === s.id">
                  <!-- 编辑模式 -->
                  <span class="step-num">{{ idx + 1 }}</span>
                  <select v-model="editDraft.type" class="xs">
                    <option v-for="t in STEP_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
                  </select>
                  <template v-if="editTypeMeta?.needXY">
                    <input type="number" v-model.number="editDraft.x" placeholder="X" class="xs" />
                    <input type="number" v-model.number="editDraft.y" placeholder="Y" class="xs" />
                  </template>
                  <template v-if="editTypeMeta?.needButton">
                    <select v-model="editDraft.button" class="xs">
                      <option value="left">左键</option>
                      <option value="right">右键</option>
                    </select>
                  </template>
                  <template v-if="editTypeMeta?.needKey">
                    <input type="text" v-model="editDraft.key" placeholder="按键" class="sm" />
                  </template>
                  <template v-if="editTypeMeta?.needText">
                    <input type="text" v-model="editDraft.text" placeholder="文本" class="grow" />
                  </template>
                  <template v-if="editTypeMeta?.needTitle">
                    <select v-model="editDraft.title" class="sm" @focus="refreshWindowTitles">
                      <option v-for="t in windowTitleOptions(editDraft.title)" :key="t" :value="t">{{ t }}</option>
                    </select>
                    <button class="ghost xs" @click="refreshWindowTitles" type="button" title="刷新窗口列表">↻</button>
                  </template>
                  <input type="number" v-model.number="editDraft.delayMs" placeholder="延迟 ms" min="0" step="50" class="xs" title="执行前延迟" />
                  <div class="step-actions">
                    <button class="primary xs" @click="saveEdit(idx)" title="保存">✓</button>
                    <button class="ghost xs" @click="cancelEdit()" title="取消">×</button>
                  </div>
                </template>
                <template v-else>
                  <!-- 只读 -->
                  <span class="step-num">{{ idx + 1 }}</span>
                  <span class="step-type" :class="['t-' + s.type]">{{ typeBadge(s.type) }}</span>
                  <span class="step-desc">{{ formatStep(s) }}</span>
                  <span class="step-delay" v-if="s.delayMs > 0">+{{ s.delayMs }}ms</span>
                  <div class="step-actions">
                    <button class="ghost xs" @click="startAddAt(idx + 1)" :disabled="isRunning || addingAtIdx !== null" title="在后面插入新步骤">➕</button>
                    <button class="ghost xs" @click="startEdit(idx)" :disabled="isRunning" title="编辑">✎</button>
                    <button class="ghost xs" @click="moveUp(idx)" :disabled="isRunning || idx === 0" title="上移">↑</button>
                    <button class="ghost xs" @click="moveDown(idx)" :disabled="isRunning || idx === steps.length - 1" title="下移">↓</button>
                    <button class="ghost xs danger" @click="deleteStep(idx)" :disabled="isRunning" title="删除">×</button>
                  </div>
                </template>
              </div>

              <!-- 步骤后面的添加面板(addingAtIdx === idx + 1) -->
              <div v-if="addingAtIdx === idx + 1" class="step-add-panel">
                <div class="add-panel-title">↓ 插入到第 {{ idx + 1 }} 步之后(将成为新的第 {{ idx + 2 }} 步)</div>
                <div class="add-form">
                  <div class="add-row">
                    <label>类型</label>
                    <select v-model="draft.type">
                      <option v-for="t in STEP_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
                    </select>
                  </div>
                  <template v-if="currentTypeMeta.needXY">
                    <div class="add-row">
                      <label>X / Y</label>
                      <div class="xy">
                        <input type="number" v-model.number="draft.x" placeholder="X" />
                        <input type="number" v-model.number="draft.y" placeholder="Y" />
                        <button type="button" class="ghost" @click="captureCurrentPos" :disabled="isRunning">⤵ 取当前位置</button>
                      </div>
                    </div>
                  </template>
                  <template v-if="currentTypeMeta.needButton">
                    <div class="add-row">
                      <label>按键</label>
                      <div class="seg">
                        <label><input type="radio" v-model="draft.button" value="left" /> 左键</label>
                        <label><input type="radio" v-model="draft.button" value="right" /> 右键</label>
                      </div>
                    </div>
                  </template>
                  <template v-if="currentTypeMeta.needKey">
                    <div class="add-row">
                      <label>按键</label>
                      <input type="text" v-model="draft.key" placeholder='如 a / Enter / ctrl+s / shift+F5' />
                    </div>
                  </template>
                  <template v-if="currentTypeMeta.needText">
                    <div class="add-row">
                      <label>文本</label>
                      <input type="text" v-model="draft.text" placeholder="要逐字符输入的文本" />
                    </div>
                  </template>
                  <template v-if="currentTypeMeta.needTitle">
                    <div class="add-row">
                      <label>窗口标题</label>
                      <div class="title-row">
                        <select v-model="draft.title" class="grow">
                          <option value="" disabled>-- 选择窗口 --</option>
                          <option v-for="t in windowTitles" :key="t" :value="t">{{ t }}</option>
                        </select>
                        <button type="button" class="ghost" @click="refreshWindowTitles" :disabled="isRunning" title="刷新窗口列表">↻</button>
                      </div>
                      <small class="hint">共 {{ windowTitles.length }} 个可见窗口,下拉选择即作为匹配关键字</small>
                    </div>
                  </template>
                  <div class="add-row">
                    <label>延迟 (ms)</label>
                    <input type="number" v-model.number="draft.delayMs" :placeholder="String(defaultStepDelay)" />
                  </div>
                </div>
                <div class="add-actions">
                  <button class="primary" @click="submitAdd">✓ 添加</button>
                  <button class="ghost" @click="cancelAdd">× 取消</button>
                </div>
              </div>
            </template>

            <!-- 末尾插入的添加面板(addingAtIdx === steps.length) -->
            <div v-if="addingAtIdx !== null && addingAtIdx >= steps.length" class="step-add-panel">
              <div class="add-panel-title">↓ 插入到末尾(将成为新的第 {{ steps.length + 1 }} 步)</div>
              <div class="add-form">
                <div class="add-row">
                  <label>类型</label>
                  <select v-model="draft.type">
                    <option v-for="t in STEP_TYPES" :key="t.value" :value="t.value">{{ t.label }}</option>
                  </select>
                </div>
                <template v-if="currentTypeMeta.needXY">
                  <div class="add-row">
                    <label>X / Y</label>
                    <div class="xy">
                      <input type="number" v-model.number="draft.x" placeholder="X" />
                      <input type="number" v-model.number="draft.y" placeholder="Y" />
                      <button type="button" class="ghost" @click="captureCurrentPos" :disabled="isRunning">⤵ 取当前位置</button>
                    </div>
                  </div>
                </template>
                <template v-if="currentTypeMeta.needButton">
                  <div class="add-row">
                    <label>按键</label>
                    <div class="seg">
                      <label><input type="radio" v-model="draft.button" value="left" /> 左键</label>
                      <label><input type="radio" v-model="draft.button" value="right" /> 右键</label>
                    </div>
                  </div>
                </template>
                <template v-if="currentTypeMeta.needKey">
                  <div class="add-row">
                    <label>按键</label>
                    <input type="text" v-model="draft.key" placeholder='如 a / Enter / ctrl+s / shift+F5' />
                  </div>
                </template>
                <template v-if="currentTypeMeta.needText">
                  <div class="add-row">
                    <label>文本</label>
                    <input type="text" v-model="draft.text" placeholder="要逐字符输入的文本" />
                  </div>
                </template>
                <template v-if="currentTypeMeta.needTitle">
                  <div class="add-row">
                    <label>窗口标题</label>
                    <div class="title-row">
                      <select v-model="draft.title" class="grow">
                        <option value="" disabled>-- 选择窗口 --</option>
                        <option v-for="t in windowTitles" :key="t" :value="t">{{ t }}</option>
                      </select>
                      <button type="button" class="ghost" @click="refreshWindowTitles" :disabled="isRunning" title="刷新窗口列表">↻</button>
                    </div>
                    <small class="hint">共 {{ windowTitles.length }} 个可见窗口,下拉选择即作为匹配关键字</small>
                  </div>
                </template>
                <div class="add-row">
                  <label>延迟 (ms)</label>
                  <input type="number" v-model.number="draft.delayMs" :placeholder="String(defaultStepDelay)" />
                </div>
              </div>
              <div class="add-actions">
                <button class="primary" @click="submitAdd">✓ 添加</button>
                <button class="ghost" @click="cancelAdd">× 取消</button>
              </div>
            </div>
          </div>
          <div class="empty" v-else>暂无步骤。点上方「➕ 添加」或用「录制」录一段。</div>
        </section>
      </div>
    </div>
  </div>
</template>
