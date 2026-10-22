// 模型商店的纯逻辑层:字节/时长格式化、脚本协议行解析(进度/交互提示)、
// 下载测速、日志累积、卡片检索与状态归一。
//
// 这里刻意不含 JSX、不含 React,也不碰 platform/tauri —— 全部是可直接单测的
// 纯函数。视图层(ModelStoreView.jsx / useLmmTasks.js)只负责把这些结果渲染
// 出来或驱动轮询。
//
// 依赖方向 features -> shared/components/platform,不得反向引用 app/。

// 日志面板最多保留的行数。长部署会打出上万行,全留在内存里只会把界面拖垮,
// 而用户真正要看的永远是最后那几十行。
const MAX_LOG_LINES = 500;

// 算网速用的采样窗口:太短会因为脚本两秒一次的采样抖得厉害,太长又反应迟钝。
const SPEED_WINDOW_MS = 20000;

// 长轮询挂起的秒数。服务端没有新行时最多挂这么久再返回,所以轮询循环不是忙等。
const LOG_WAIT_SECONDS = 25;

const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

/**
 * 人类可读的字节数。单位是国际通用符号,不需要进 i18n 词典。
 * @param {number} bytes - 字节数。
 * @returns {string} 形如 `12.3GB` 的字符串。
 */
function formatBytes(bytes) {
  let value = Number(bytes) || 0;
  for (const unit of BYTE_UNITS) {
    if (Math.abs(value) < 1024) {
      return unit === 'B' ? `${Math.round(value)}${unit}` : `${value.toFixed(1)}${unit}`;
    }
    value /= 1024;
  }
  return `${value.toFixed(1)}PB`;
}

/**
 * 把剩余秒数拆成可以交给 i18n 模板的结构,而不是在这里拼中文。
 * @param {number} seconds - 剩余秒数。
 * @returns {{unit:'sec'|'min'|'hour', value:number, rest:number}|null} 拆解结果;非法输入返回 null。
 */
function splitDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return null;
  const total = Math.round(seconds);
  if (total < 60) return { unit: 'sec', value: total, rest: 0 };
  const minutes = Math.floor(total / 60);
  if (minutes < 60) return { unit: 'min', value: minutes, rest: 0 };
  return { unit: 'hour', value: Math.floor(minutes / 60), rest: minutes % 60 };
}

/**
 * 脚本约定:`LMM_PROGRESS_BYTES=已下载/总量`。
 *
 * 已下载会夹到总量以内。脚本那边本来就该算准,这里是最后一道兜底:真机上出现过
 * 「51.0GB / 30.6GB,进度 100% 还在转」——并发下载留下的孤儿临时块被重复统计了。
 * 哪怕将来某张卡的脚本又算错,界面也不该把这种数字摆给用户看。
 *
 * # 🔴 这个正则**刻意没有 `^` 锚定**，不要"顺手"加上
 *
 * 宿主会给日志行加阶段前缀（`[下载模型] `、`[安装引擎] `，见 Rust 侧
 * `script_runner::prefix_log_line`）。现在那个函数会放协议行原样通过，但**历史上
 * 不会**——设备上跑着的旧版本、以及任何将来新加的中继层，都可能再给进度行套一层
 * 前缀。不锚定是这条链路唯一的容错点：加上 `^` 之后进度条会**静默**全灭（没有报错、
 * 没有异常，只是永远不出现进度），而这类故障要在真机上盯着一次几十 GB 的下载才看
 * 得出来。回归测试见 model_store_logic.test.mjs 里喂 `[下载模型] LMM_PROGRESS_BYTES=1/2`
 * 那一条。
 *
 * 同理下面的 `parseStageLine` / `parseStepLine` / `parseDockerLayerLine` 也都不锚定。
 * 反过来 `parsePromptLine` **必须**锚定（见它自己的说明）。
 * @param {string} line - 一行脚本输出。
 * @returns {{done:number, total:number}|null} 解析结果;不是进度行则返回 null。
 */
function parseProgressLine(line) {
  const matched = /LMM_PROGRESS_BYTES=(\d+)\/(\d+)/.exec(String(line ?? ''));
  if (!matched) return null;
  const total = Number(matched[2]);
  const done = Number(matched[1]);
  return { done: total > 0 ? Math.min(done, total) : done, total };
}

/**
 * 脚本约定:`LMM_STAGE=<人话阶段名>`。
 *
 * 装引擎有好几个依赖包、好几个动作（拉镜像 / 装 Python 依赖 / 解交付包 / 起容器），
 * 每一段的字节计数各自从零开始。没有阶段名的时候任务卡副标题退化成"最后一行日志",
 * 用户看到的就是 docker 自己的多层并行进度在原地乱跳。
 * @param {string} line - 一行脚本输出。
 * @returns {string|null} 阶段名;不是阶段行则返回 null。
 */
function parseStageLine(line) {
  const matched = /LMM_STAGE=(.*)$/.exec(String(line ?? ''));
  if (!matched) return null;
  const name = matched[1].trim();
  return name || null;
}

/**
 * 脚本约定:`LMM_STEP=<第几步>/<共几步>`。
 *
 * **给拿不到字节分母的阶段用**。`pip install` 的依赖总量要等依赖解析完才知道,在那
 * 之前不存在任何可信的分母;宁可给一个诚实的「第 2 步 / 共 4 步」,也不要编一个假分母
 * 让进度条先冲到 90% 再倒退——那比没有进度条更伤信任。
 * @param {string} line - 一行脚本输出。
 * @returns {{index:number, total:number}|null} 解析结果;不是步骤行则返回 null。
 */
function parseStepLine(line) {
  const matched = /LMM_STEP=(\d+)\/(\d+)/.exec(String(line ?? ''));
  if (!matched) return null;
  const total = Number(matched[2]);
  if (total <= 0) return null;
  return { index: Math.min(Number(matched[1]), total), total };
}

// docker 的 `HumanSize` 用 1000 进位（`GB`）,`BytesSize` 用 1024 进位（`GiB`）。
// 两套都认,按它自己的语义换算——把 `4.07GB` 当成 1024 进位会让分母凭空大 7%,
// 而这个分母正是进度条的分母。
const DOCKER_SIZE_UNITS = {
  b: 1,
  kb: 1000,
  mb: 1000 ** 2,
  gb: 1000 ** 3,
  tb: 1000 ** 4,
  kib: 1024,
  mib: 1024 ** 2,
  gib: 1024 ** 3,
  tib: 1024 ** 4,
};

/**
 * 把 docker 打出来的 `1.967GB` 之类换成字节数。
 * @param {string} text - 带单位的大小。
 * @returns {number} 字节数;认不出来时 0。
 */
function parseDockerSize(text) {
  const matched = /^([\d.]+)\s*([a-zA-Z]{1,3})$/.exec(String(text ?? '').trim());
  if (!matched) return 0;
  const scale = DOCKER_SIZE_UNITS[matched[2].toLowerCase()];
  const value = Number(matched[1]);
  if (!scale || !Number.isFinite(value)) return 0;
  return value * scale;
}

// 宿主给日志行加的阶段前缀（`[安装引擎] ` / `[下载模型] `）。层聚合要先把它剥掉,
// 否则 `<层 id>:` 那个判据落不到行首。
const STAGE_PREFIX_RE = /^\[[^\]]{1,24}\]\s+/;

// `<12 位十六进制层 id>: <状态>`。docker pull 的每一层都按这个形状刷新自己那一行。
const DOCKER_LAYER_RE = /^([0-9a-f]{8,64}):\s+(.+)$/;

// `Downloading [=====>    ]  1.967GB/4.07GB` 里的两个数。
const DOCKER_LAYER_BYTES_RE = /\[[=>\s]*\]\s+([\d.]+\s*[a-zA-Z]{1,3})\s*\/\s*([\d.]+\s*[a-zA-Z]{1,3})/;

// 这些状态意味着这一层已经不再占用下行带宽了。`Already exists` 是本机已有,压根没下。
const DOCKER_LAYER_DONE = ['download complete', 'pull complete', 'already exists', 'verifying checksum'];

// 最多跟踪多少层。真实镜像几十层就到顶了;设上限是因为层 id 来自**卡片脚本的输出**,
// 而卡片来自网络:一个被投毒的脚本可以打出无限多个假层 id,把这张表撑成内存泄漏。
const MAX_TRACKED_LAYERS = 200;

/**
 * 认出 docker pull 的逐层刷屏行。
 *
 * 用户的原话:「现在显示出来的日志别说用户看得头痛,我看到都一脸懵逼」。截图上刷的
 * 就是这些——十几行 `ae5a106b6076: Pulling fs layer`,加上多层并行的
 * `7014a14111a1: Downloading [====>] 1.967GB/4.07GB` 一起跳。它们**不进日志**,由
 * `layerSummary` 聚合成一行原地更新的汇总。
 * @param {string} line - 一行脚本输出。
 * @returns {{id:string, status:string, done:number, total:number, complete:boolean}|null} 解析结果。
 */
function parseDockerLayerLine(line) {
  const body = String(line ?? '').replace(STAGE_PREFIX_RE, '').trim();
  const matched = DOCKER_LAYER_RE.exec(body);
  if (!matched) return null;
  const status = matched[2].trim();
  const bytes = DOCKER_LAYER_BYTES_RE.exec(status);
  const lower = status.toLowerCase();
  return {
    id: matched[1],
    status,
    done: bytes ? parseDockerSize(bytes[1]) : 0,
    total: bytes ? parseDockerSize(bytes[2]) : 0,
    complete: DOCKER_LAYER_DONE.some((done) => lower.startsWith(done)),
  };
}

/**
 * 把一次层状态更新并进层表。
 *
 * 已知的总量要**记住**:`Downloading` 那一行带着 `x/y`,而紧随其后的
 * `Verifying Checksum` / `Download complete` 不带——不记住的话分母会在最后一刻塌掉。
 * @param {Record<string, object>|null} layers - 现有层表。
 * @param {{id:string, status:string, done:number, total:number, complete:boolean}} update - 本次更新。
 * @returns {Record<string, object>} 新层表（不改入参）。
 */
function applyLayerUpdate(layers, update) {
  const next = { ...(layers || {}) };
  const previous = next[update.id];
  if (!previous && Object.keys(next).length >= MAX_TRACKED_LAYERS) return next;
  const total = update.total > 0 ? update.total : (previous?.total ?? 0);
  const done = update.complete ? total : Math.max(update.done, previous?.done ?? 0);
  next[update.id] = { status: update.status, total, done, complete: update.complete };
  return next;
}

/**
 * 层表的汇总:一共几层、完成几层、已下多少字节、总共多少字节。
 *
 * # 「统计引擎总包大小」这件事能做到什么程度
 *
 * **拿不到 pull 之前的总量。** docker 只在每一层开始下载时才打出那一层的 `x/y`,
 * 所以这里的 `total` 是「**到目前为止已经露面的**层的总和」,会随 pull 推进往上长。
 * 真要在 pull 之前知道总量,只能由**卡片脚本**去问一次 registry 的 manifest（各层
 * `size` 之和）并用 `LMM_PROGRESS_BYTES` 上报——那要多一次网络请求和 registry 认证,
 * 归卡片脚本管,见 pinvou3-ops/docs 里的规范增补草案。
 *
 * 所以这份汇总只在脚本**没有**上报 `LMM_PROGRESS_BYTES` 时兜底,而且界面必须标成
 * 「估算」:分母会长,百分比会往回跳。宁可让用户看见"这是估的",也不要把一个会倒退的
 * 数字当成确定进度。
 * @param {Record<string, object>|null} layers - 层表。
 * @returns {{count:number, complete:number, done:number, total:number}|null} 汇总;没有层时 null。
 */
function layerSummary(layers) {
  const entries = Object.values(layers || {});
  if (entries.length === 0) return null;
  let done = 0;
  let total = 0;
  let complete = 0;
  for (const layer of entries) {
    done += Number(layer.done) || 0;
    total += Number(layer.total) || 0;
    if (layer.complete) complete += 1;
  }
  return { count: entries.length, complete, done, total: Math.max(total, done) };
}

const PROMPT_KINDS = ['text', 'password', 'confirm'];

/**
 * 脚本约定:`LMM_PROMPT=<kind>|<提示语>`,脚本打印这一行后阻塞等答案。
 *
 * kind 认 text / password / confirm 三种,不认识的一律按 text 处理 —— 卡片脚本
 * 可能比应用新,不该因为多了一种输入类型就整个跑不动。
 *
 * 另有表单式 `LMM_PROMPT_FORM=<一行 JSON>`:一次问齐多项。双机部署要问从机 IP、
 * 用户名、密码、本机密码,一问一答会连弹四个框,而那个流程的目标恰恰是「填一次
 * 就不用管了」。
 * @param {string} line - 一行脚本输出。
 * @returns {{kind:string, message:string, title?:string, fields?:Array<object>}|null} 解析结果;不是提示行则返回 null。
 */
function parsePromptLine(line) {
  const trimmed = String(line ?? '').trim();

  const form = /^LMM_PROMPT_FORM=(.*)$/.exec(trimmed);
  if (form) {
    try {
      const spec = JSON.parse(form[1]);
      const fields = (spec.fields || []).filter((field) => field && field.id);
      // 没有字段的表单等于没问,退化成普通日志行比弹一个空框强。
      if (fields.length === 0) return null;
      // message:为什么又问一遍(地址连不上、密码不对……)。不说原因就重新弹同一张
      // 表,用户只会以为是界面卡住了。
      return { kind: 'form', title: spec.title || '', message: spec.message || '', fields };
    } catch {
      // 脚本把 JSON 写坏了。不能就这么把这行吞掉 —— 那样用户会对着一个不动的进度条
      // 等到超时;退化成一条普通日志,至少能看见发生了什么。
      return null;
    }
  }

  const matched = /^LMM_PROMPT=(.*)$/.exec(trimmed);
  if (!matched) return null;
  const rest = matched[1];
  const separator = rest.indexOf('|');
  if (separator < 0) return { kind: 'text', message: rest.trim() };
  const kind = rest.slice(0, separator).trim().toLowerCase();
  return {
    kind: PROMPT_KINDS.includes(kind) ? kind : 'text',
    message: rest.slice(separator + 1).trim(),
  };
}

/**
 * 按采样窗口算下载速度(字节/秒)。
 * @param {Array<{t:number, done:number}>} samples - 时间戳升序的采样点。
 * @returns {number|null} 速度;样本不足或倒退(换算基准变了)时返回 null。
 */
function computeSpeed(samples) {
  if (!Array.isArray(samples) || samples.length < 2) return null;
  const last = samples[samples.length - 1];
  const first = samples.find((sample) => last.t - sample.t <= SPEED_WINDOW_MS) ?? samples[0];
  const elapsed = (last.t - first.t) / 1000;
  const delta = last.done - first.done;
  if (elapsed <= 0 || delta < 0) return null;
  return delta / elapsed;
}

/**
 * 丢掉窗口之外的老样本,避免采样数组随长下载无限增长。
 * @param {Array<{t:number, done:number}>} samples - 现有采样点。
 * @param {number} now - 当前时间戳。
 * @returns {Array<{t:number, done:number}>} 裁剪后的采样点。
 */
function trimSamples(samples, now) {
  const kept = [...samples];
  while (kept.length > 2 && now - kept[0].t > SPEED_WINDOW_MS * 2) kept.shift();
  return kept;
}

/**
 * 把长轮询取回的一批行折叠进任务状态。
 *
 * 这是整条链路上唯一需要仔细对待的地方,所以单独抽成纯函数:进度行和提示行都是
 * 给界面看的控制信息、不该进日志(进度行两秒一条,不拦下来会把真正有用的报错刷没),
 * 而它们的副作用(测速采样、弹输入框)又必须按出现顺序生效。
 *
 * 除了协议行,还有两类**噪声**在这里被拦下来,不进日志:
 *
 * 1. **docker 的逐层刷屏**(`<层 id>: Pulling fs layer` / `Waiting` /
 *    `Downloading [====>] 1.9GB/4.07GB` …)。一个镜像几十层,每层每秒重绘一次,
 *    逐行追加的结果就是用户说的「滚动的闪日志,这样很乱」。它们聚合进 `layers`,
 *    由界面渲染成一行原地更新的汇总。
 * 2. **紧挨着的重复行**。脚本用 `\r` 原地重绘进度时,宿主按 `\r` 切行
 *    (`script_runner::drain_lines`),同一句话会来几百遍。
 *
 * 入参允许缺少 `stage` / `step` / `layers`(老的调用方和测试只给五个字段),缺了就
 * 当没有——多一个字段不该让既有调用方炸掉。
 * @param {{lines:Array<string>, progress:object|null, speed:number|null, prompt:object|null, samples:Array<object>, stage?:string|null, step?:object|null, layers?:object|null}} state - 折叠前的任务状态。
 * @param {Array<string>} chunk - 本次取回的行。
 * @param {number} now - 当前时间戳(测速用,注入方便测试)。
 * @returns {{lines:Array<string>, progress:object|null, speed:number|null, prompt:object|null, samples:Array<object>, stage:string|null, step:object|null, layers:object|null}} 折叠后的任务状态。
 */
function reduceLogChunk(state, chunk, now) {
  let { progress, speed, prompt } = state;
  let samples = state.samples;
  let stage = state.stage ?? null;
  let step = state.step ?? null;
  let layers = state.layers ?? null;
  let retainedNativeProcess = state.retainedNativeProcess === true;
  const lines = [...state.lines];

  const sample = (done) => {
    samples = trimSamples([...samples, { t: now, done }], now);
    speed = computeSpeed(samples);
  };

  for (const raw of chunk || []) {
    const line = String(raw ?? '');
    if (line === 'LMM_RETAINED_NATIVE_PROCESS=1') {
      retainedNativeProcess = true;
      continue;
    }
    const parsedPrompt = parsePromptLine(line);
    if (parsedPrompt) {
      prompt = parsedPrompt;
      continue;
    }
    const parsedStage = parseStageLine(line);
    if (parsedStage !== null) {
      // 换阶段就把进度、层表、测速采样一起清掉。下一阶段的字节计数从零重新开始,
      // 跨阶段做差算出来的不是网速而是噪声(上一阶段的总量除以两次采样的间隔),
      // 而留着上一阶段的分母会让新阶段一开始就显示 99%。
      if (parsedStage !== stage) {
        samples = [];
        speed = null;
        progress = null;
        layers = null;
        // step 是阶段的从属物,换阶段一并清——不清的话新阶段开头顶着上一阶段的
        // 「第 N 步/共 M 步」假步进(评审 P3,探针实证)。
        step = null;
      }
      stage = parsedStage;
      continue;
    }
    const parsedStep = parseStepLine(line);
    if (parsedStep) {
      step = parsedStep;
      continue;
    }
    const parsedProgress = parseProgressLine(line);
    if (parsedProgress) {
      progress = parsedProgress;
      sample(parsedProgress.done);
      continue;
    }
    const parsedLayer = parseDockerLayerLine(line);
    if (parsedLayer) {
      layers = applyLayerUpdate(layers, parsedLayer);
      // 只在脚本没上报字节进度时拿层聚合去测速:两套分子混进同一串采样里,
      // 差值算出来毫无意义。
      if (!progress) {
        const summary = layerSummary(layers);
        if (summary && summary.total > 0) sample(summary.done);
      }
      continue;
    }
    if (lines.length > 0 && lines[lines.length - 1] === line) continue;
    lines.push(line);
  }

  return {
    lines: lines.length > MAX_LOG_LINES ? lines.slice(-MAX_LOG_LINES) : lines,
    progress,
    speed,
    prompt,
    samples,
    stage,
    step,
    layers,
    retainedNativeProcess,
  };
}

/**
 * 进度百分比(0-100),没有总量时返回 null(界面据此改用不确定态)。
 * @param {{done:number, total:number}|null} progress - 进度。
 * @returns {number|null} 百分比。
 */
function progressPercent(progress) {
  if (!progress) return null;
  const total = Number(progress.total);
  if (!Number.isFinite(total) || total <= 0) return null;
  return Math.min(100, (Number(progress.done) / total) * 100);
}

/**
 * 任务卡该画成什么样的进度。
 *
 * 三档,按可信度从高到低,**绝不编假分母**:
 *
 * 1. `bytes` + `estimated:false`——脚本自己上报的 `LMM_PROGRESS_BYTES`。分母是卡片
 *    元数据里的真实体积(下权重)或这一阶段的包总量(装引擎),可信。
 * 2. `bytes` + `estimated:true`——只有 docker 的层聚合可用。**分母会随 pull 推进往上
 *    长**(docker 要等一层开始下载才报那一层的大小),所以百分比会往回跳,界面必须
 *    把它标成估算。
 * 3. `step`——连字节都没有(`pip install` 的依赖总量要等解析完才知道)。给「第 2 步 /
 *    共 4 步」这种粗粒度,而不是假装有字节级进度。
 *
 * 三档都不成立就返回 null,界面退化成不确定态(转圈 + 阶段名),这也是诚实的。
 * @param {object|null} task - 任务。
 * @returns {{kind:'bytes'|'step', percent:number, awaitingCompletion:boolean, estimated:boolean, done?:number, total?:number, index?:number, layers?:object}|null} 进度视图。
 */
function taskProgressView(task) {
  // Byte counters describe transfer, not successful completion. Old scripts
  // can reach their advertised size early (e.g. by counting stale partials).
  // Keep the raw counter available, but let the UI switch to an indeterminate
  // state until the backend actually finishes instead of promising 100% / 0s.
  const pendingCompletion = (percent) => Number(percent.toFixed(1)) >= 100 && task?.status !== 'done';
  const percent = progressPercent(task?.progress);
  if (percent !== null) {
    return {
      kind: 'bytes',
      percent,
      awaitingCompletion: pendingCompletion(percent),
      estimated: false,
      done: Number(task.progress.done),
      total: Number(task.progress.total),
    };
  }
  const summary = layerSummary(task?.layers);
  if (summary && summary.total > 0) {
    return {
      kind: 'bytes',
      percent: Math.min(100, (summary.done / summary.total) * 100),
      awaitingCompletion: pendingCompletion((summary.done / summary.total) * 100),
      estimated: true,
      done: summary.done,
      total: summary.total,
      layers: summary,
    };
  }
  const step = task?.step;
  if (step && Number(step.total) > 0) {
    return {
      kind: 'step',
      percent: Math.min(100, (Number(step.index) / Number(step.total)) * 100),
      awaitingCompletion: pendingCompletion((Number(step.index) / Number(step.total)) * 100),
      estimated: false,
      index: Number(step.index),
      total: Number(step.total),
    };
  }
  return null;
}

/**
 * 按当前速度估算剩余秒数。
 * @param {{done:number, total:number}|null} progress - 进度。
 * @param {number|null} speed - 字节/秒。
 * @returns {number|null} 剩余秒数;算不出时返回 null。
 */
function etaSeconds(progress, speed) {
  if (!progress || !speed || speed <= 0) return null;
  const remaining = Number(progress.total) - Number(progress.done);
  if (!Number.isFinite(remaining) || remaining <= 0) return null;
  return remaining / speed;
}

/**
 * 把 `model_store_availability` 的返回归一成界面要展示的引导态。
 *
 * 只有三档。原来有五档（loading / not-installed / stopped / error / ready），
 * 因为模型商店当时是一个**独立进程**（LMM sidecar）:它可能没装、可能没起来、
 * 可能起不来,每一种都要给用户不同的话和不同的按钮。
 *
 * 2026-09-18 改成原生实现之后那三档一起消失了——代码就在本进程里,不存在"没装"
 * 和"没起来"。剩下的只有:还没问到（loading）、这个平台没有这个功能（unsupported，
 * 编译期就定死）、可用（ready）。
 * @param {{available?:boolean, reason?:string|null}|null} availability - 可用性。
 * @returns {'loading'|'unsupported'|'ready'} 引导态。
 */
function storeGuideState(availability) {
  if (!availability) return 'loading';
  return availability.available ? 'ready' : 'unsupported';
}

/**
 * 已下载模型的展示状态。运行中优先于已部署:用户关心的是「现在能不能调」。
 * @param {{running?:boolean, status?:string}} record - 库记录。
 * @returns {'running'|'deployed'|'pending'} 展示状态。
 */
function libraryStatusOf(record) {
  if (!record) return 'pending';
  if (record.running) return 'running';
  return record.status === 'deployed' ? 'deployed' : 'pending';
}

/**
 * 商店卡片检索。名称/提供方/引擎/量化四个字段都能命中 —— 用户记得住的往往不是
 * 全名,而是「那个 qwen 的」「那个 awq 的」。
 * @param {Array<object>|null} cards - 卡片列表。
 * @param {string} query - 检索词。
 * @returns {Array<object>} 命中的卡片。
 */
function filterStoreCards(cards, query) {
  const list = Array.isArray(cards) ? cards : [];
  const keyword = String(query ?? '').trim().toLowerCase();
  if (!keyword) return list;
  return list.filter((card) => [card.name, card.org, card.engine_name, card.quantization, card.description]
    .some((field) => String(field ?? '').toLowerCase().includes(keyword)));
}

/** 一个模型在用户眼里的六种状态，按「离能用有多远」从近到远排。 */
const MODEL_STATES = ['running', 'deployed', 'downloaded', 'incomplete', 'cleanup_required', 'available'];

/**
 * 库记录落在哪一档状态。
 * @param {object} record - 库记录。
 * @returns {'running'|'deployed'|'downloaded'|'incomplete'|'cleanup_required'} 状态。
 */
function libraryStateOf(record) {
  if (record?.running) return 'running';
  // “删除模型数据”跑过、但后验扫描仍发现本卡文件时，不能把它伪装成“可下载”或
  // “已下载”。文件仍可见可管理，下一步是继续清理（或明确仅移除列表记录）。
  if (record?.status === 'cleanup_required') return 'cleanup_required';
  // “已下载”必须有一次成功脚本校验留下的物理凭据。升级前的记录没有
  // weights_ready_at；部署刚发现权重缺失的记录也会清掉它。两类都应当让用户
  // “继续下载”，即使旧数据库仍写着 deployed 也不能例外。卡片脚本会复用
  // 已经落盘的分片，而不是从头再传。
  if (record?.status === 'incomplete' || !record?.weights_ready_at) return 'incomplete';
  if (record?.status === 'deployed') return 'deployed';
  return 'downloaded';
}

/**
 * 卸载对话框该摆几个选项。
 *
 * 「卸载」的意思是把模型从引擎上撤下来。卡片只是下载在本机、从来没部署过（或者
 * 刚刚被卸载过）时，这件事根本无从做起：没有服务、没有容器、没有部署配置。这时
 * 再摆一个「卸载」按钮，用户点下去只会收到一个什么都没发生的成功提示——而他真正
 * 想要的、能腾出磁盘的那个动作叫「删除」。
 *
 * 判据跟 `libraryStateOf` 同源（running / status 两个字段），免得两处对"算不算
 * 已部署"给出不同答案。
 * @param {object|null} record - 库记录。
 * @returns {'uninstall-or-delete'|'delete-only'} 对话框形态。
 */
function uninstallOptionsFor(record) {
  const state = libraryStateOf(record);
  if (state === 'cleanup_required') return 'cleanup-only';
  return state === 'downloaded' || state === 'incomplete' ? 'delete-only' : 'uninstall-or-delete';
}

/** 算作「这个字段是空的」的几种取值。库记录里三种都出现过。 */
const BLANK_VALUES = [undefined, null, ''];

/** 合并时从商店卡片补进来的字段——库记录缺了才补。 */
const MERGEABLE_FIELDS = [
  'name', 'org', 'description', 'engine_name', 'engine_version',
  'quantization', 'size_bytes', 'deploy_topology',
  // 档位只存在于商店卡片上，库记录（sqlite）里没有这一列。所以对已下载的模型，
  // 它必须靠这次合并补进来；商店列表拉不到（离线、卡片已下架）时补不上，
  // weightVerificationOf 会按 none 处理——多出一条黄条，而不是少一条。
  'weight_verification',
];


/**
 * 把「模型市场」和「我的模型」并成一份列表。
 *
 * 分成两个 Tab 是照搬 LMM 桌面端的信息架构，而那是**实现视角**：商店来自远端
 * 目录、本地库来自 sqlite，两份数据两张表。用户视角里它们是同一件事——
 * 「这台机器上有哪些模型，各自到哪一步了」。分成两半之后，同一个模型会在下载完成
 * 的瞬间从左边消失、在右边出现，这种横跳比合在一起难理解得多。
 *
 * 合并靠 `card_id`：商店卡片的 `id` 和库记录的 `card_id` 是同一个标识。两边都有时
 * **以库记录为准**——它带着运行态（running / status / endpoint_url），商店那份只有
 * 静态描述；商店那份仅用来补库记录没有的字段（描述通常只有商店有）。
 *
 * 顺带把两套字段名归一成商店那套（库记录叫 `card_name`，商店叫 `name`），
 * 让卡片组件只需要认一种形状。
 * @param {Array<object>|null} cards - 商店卡片列表。
 * @param {{models?: Array<object>}|null} library - 本地库。
 * @returns {Array<object>} 合并后的列表，每项带 `state` 和 `record`（未下载时为 null）。
 */
function mergeModels(cards, library) {
  const storeList = Array.isArray(cards) ? cards : [];
  const records = Array.isArray(library?.models) ? library.models : [];
  const byId = new Map();

  for (const record of records) {
    if (!record?.card_id) continue;
    byId.set(record.card_id, {
      id: record.card_id,
      name: record.card_name || record.model_name || record.card_id,
      org: record.org,
      description: record.description,
      engine_name: record.engine_name,
      engine_version: record.engine_version,
      quantization: record.quantization,
      size_bytes: record.size_bytes,
      deploy_topology: record.deploy_topology,
      source_repo_id: record.source_repo_id,
      state: libraryStateOf(record),
      record,
    });
  }

  for (const card of storeList) {
    if (!card?.id) continue;
    const existing = byId.get(card.id);
    if (!existing) {
      byId.set(card.id, { ...card, state: 'available', record: null });
      continue;
    }
    for (const key of MERGEABLE_FIELDS) {
      // 空字符串也算缺：库记录里 description 经常是 '' 而不是 null。
      if (BLANK_VALUES.includes(existing[key])) existing[key] = card[key];
    }
  }

  return [...byId.values()];
}

/**
 * 按关键词和状态过滤合并后的列表。
 *
 * `filter` 的 `installed` 指「本机已经有权重的」——下载过就算，不管部署没部署。
 * 这是用户最常问的那个问题（「我这儿都有啥」），比逐档筛有用。
 * @param {Array<object>} models - mergeModels 的产物。
 * @param {{query?: string, filter?: string}} options - 检索词与状态筛选。
 * @returns {Array<object>} 过滤后的列表。
 */
function filterModels(models, { query = '', filter = 'all' } = {}) {
  const list = Array.isArray(models) ? models : [];
  const keyword = String(query ?? '').trim().toLowerCase();
  return list.filter((model) => {
    if (filter === 'installed' && model.state === 'available') return false;
    if (filter === 'running' && model.state !== 'running') return false;
    if (!keyword) return true;
    return [model.name, model.org, model.engine_name, model.quantization, model.description]
      .some((field) => String(field ?? '').toLowerCase().includes(keyword));
  });
}

/**
 * 排序：能用的排前面，同档按名字。
 *
 * 用户进来最想看见的是「现在就能调的那个」，最不想翻的是商店里一堆还没下的。
 * 用 localeCompare 的 zh 规则，中文名才不会按码位乱排。
 * @param {Array<object>} models - 合并后的列表。
 * @returns {Array<object>} 排序后的新数组（不改入参）。
 */
function sortModels(models) {
  const rank = (state) => {
    const index = MODEL_STATES.indexOf(state);
    return index < 0 ? MODEL_STATES.length : index;
  };
  return [...(Array.isArray(models) ? models : [])].sort((a, b) => {
    const delta = rank(a.state) - rank(b.state);
    if (delta !== 0) return delta;
    return String(a.name ?? '').localeCompare(String(b.name ?? ''), 'zh');
  });
}

/**
 * 同一张卡的下载任务共用一个 key,任务面板据此拒掉重复下载。
 * @param {string} cardId - 卡片 ID。
 * @returns {string} 任务 key。
 */
const downloadTaskKey = (cardId) => `download:${cardId}`;

/**
 * 同一张卡的生命周期操作(部署/启用/停用/卸载)共用一个 key:这些操作之间也不该
 * 并发,正在启用时点停用同样会让脚本互相踩。
 * @param {string} cardId - 卡片 ID。
 * @returns {string} 任务 key。
 */
const lifecycleTaskKey = (cardId) => `card:${cardId}`;

/** 生命周期任务 key 的前缀。反推卡片 ID 时要按它来切,见 cardIdFromLifecycleKey。 */
const LIFECYCLE_KEY_PREFIX = 'card:';

/**
 * 从生命周期任务的 key 反推卡片 ID。
 *
 * 为什么要反推而不是把 card_id 直接存进任务:任务对象是 useLmmTasks 的通用结构
 * (title/kind/key/日志),它刻意不认识"模型"这个概念 —— 下载任务和生命周期任务
 * 共用同一套状态机。为一个入口往里塞业务字段,等于让任务面板开始懂模型库。
 * key 本来就是这里生成的,反函数放在同一处,格式改了也不会有人漏掉。
 * @param {string|null|undefined} key - 任务 key。
 * @returns {string|null} 卡片 ID;不是生命周期 key 时返回 null。
 */
function cardIdFromLifecycleKey(key) {
  if (typeof key !== 'string' || !key.startsWith(LIFECYCLE_KEY_PREFIX)) return null;
  const cardId = key.slice(LIFECYCLE_KEY_PREFIX.length);
  return cardId || null;
}

/**
 * 这个任务该不该露出「强制从列表移除」的入口。
 *
 * 只认「卸载失败」这一种情况,而且只在任务卡片上露 —— 强制移除不可逆、还会把
 * 几十上百 GB 的文件留在磁盘上没人管,做成常驻按钮迟早有人当成第二个卸载键按。
 * 真正的死结只出现在这一刻:脚本非零退出 → LMM 不删记录 → 卡片留在列表里但
 * 已经被掏空,重试卸载还会卡在同一处(比如双机卡的从机连不上,ssh 回 255)。
 *
 * 取消掉的任务不算:那是用户自己停的,再跑一次卸载就好,没有死结。
 * @param {object} task - 任务。
 * @returns {boolean} 是否给出逃生入口。
 */
function canForceRemove(task) {
  return !!task && task.kind === 'uninstall' && task.status === 'error' && !!cardIdFromLifecycleKey(task.key);
}

/**
 * A card may offer explicit stop after failed readiness; this is not a running/
 * healthy assertion or signal authorization. The stop script rechecks identity.
 * @param {object} task - Lifecycle task.
 * @returns {boolean} Whether the user can request a confirmed recovery stop.
 */
function canStopRetainedProcess(task) {
  return !!task && task.kind === 'enable' && task.status === 'error'
    && task.retainedNativeProcess === true && !!cardIdFromLifecycleKey(task.key);
}

/**
 * 表单提交前的必填校验。字段里 `required` 默认为真 —— 脚本问了就是要用的,
 * 空着提交只会让它在后面某一步以更难懂的方式失败。
 * @param {Array<object>} fields - 表单字段。
 * @param {object} values - 当前输入值。
 * @returns {Array<object>} 仍为空的必填字段。
 */
function missingFormFields(fields, values) {
  return (fields || []).filter(
    (field) => field.required !== false && !String(values?.[field.id] ?? '').trim()
  );
}

// 这里原来有 UNLOCK_CLICKS / UNLOCK_CLICK_GAP_MS / advanceUnlockClicks:模型商店
// 页面标题上那个「连点 7 次弹口令框」的隐藏入口,用来打开内置测试商店。内置测试
// 商店 2026-09-30 整体拿掉,这个入口和它背后的三个 Tauri 命令一起没了。

/**
 * 卸载完成那句话,按服务端回的**实际结果**选,而不是按用户点了哪个按钮。
 *
 * 后端 `uninstall_payload` 专门回了 `purged_weights` / `freed_bytes` /
 * `remaining_paths` 三个字段来防一件具体的事:卸载脚本退出 0、库记录已删,但权重目录
 * 其实没删掉。设备上 21 GB 的 Qwen3.8-27B-NVFP4 就是这么留下的。那种情况下如果按请求
 * 参数拼话,界面会说「模型权重已删除」——磁盘一个字节没少,而卡片已经从列表消失,此后
 * 没有任何界面能管到那堆文件。所以 remaining_paths 非空时必须改口。
 *
 * outcome 允许为空:响应里没带 result 时退回按"保留权重"措辞——宁可少说一句
 * "已释放",也不能凭空承诺一件没验证过的事。
 * @param {Record<string, Function>} copy - uiModelStore 词典。
 * @param {string} name - 卡片显示名。
 * @param {{purged_weights?:boolean, freed_bytes?:number, remaining_paths?:Array<string>}|null} outcome - 服务端结果。
 * @returns {string} 提示文案。
 */
function uninstallDoneText(copy, name, outcome) {
  const remaining = Array.isArray(outcome?.remaining_paths) ? outcome.remaining_paths : [];
  // 只有"本该删干净却没删干净"才算异常。仅卸载(保留权重)时权重本来就该在,
  // 服务端也不会把它们报成 remaining_paths。
  if (outcome?.purged_weights && remaining.length > 0) {
    return copy.uninstallDoneResidual(name, remaining.length);
  }
  if (outcome?.purged_weights) {
    return copy.uninstallDonePurged(name, formatBytes(outcome.freed_bytes));
  }
  return copy.uninstallDoneKept(name);
}

export {
  MAX_LOG_LINES,
  SPEED_WINDOW_MS,
  LOG_WAIT_SECONDS,
  formatBytes,
  uninstallDoneText,
  uninstallOptionsFor,
  splitDuration,
  parseProgressLine,
  parsePromptLine,
  parseStageLine,
  parseStepLine,
  parseDockerLayerLine,
  parseDockerSize,
  applyLayerUpdate,
  layerSummary,
  taskProgressView,
  computeSpeed,
  trimSamples,
  reduceLogChunk,
  progressPercent,
  etaSeconds,
  storeGuideState,
  libraryStatusOf,
  libraryStateOf,
  filterStoreCards,
  MODEL_STATES,
  mergeModels,
  filterModels,
  sortModels,
  downloadTaskKey,
  lifecycleTaskKey,
  cardIdFromLifecycleKey,
  canForceRemove,
  canStopRetainedProcess,
  missingFormFields,
};
