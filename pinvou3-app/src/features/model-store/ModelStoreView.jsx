// 模型商店:浏览 / 下载 / 在本机部署大模型。
//
// 数据来自本进程的 model_store 命令(见 model-store-api.js),本文件不直接碰
// __TAURI__。2026-09-18 之前这些数据来自一个叫 LMM 的 sidecar 进程,经 HTTP
// 管理接口取——那一层已经整个去掉了。
//
// 长操作(下载/部署/启用/停用/卸载)不阻塞界面:拿到 run_id 后由 useLmmTasks 用
// 带游标的长轮询读输出,进度和脚本交互都落在右下角的任务面板里。用户可以照常
// 翻别的模型,也可以同时下好几个(但同一张卡只能有一个)。任务表在模块级,所以
// 切走视图不影响它们继续跑;进页面时还会再问一次后端有哪些运行还活着,把失联的
// 那些接回来(见 reattachActiveRuns)。
//
// 依赖方向 features -> components/shared/platform,不得反向引用 app/。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, Database, Package, RefreshCw } from '../../components/icons.jsx';
import { EmptyState } from '../../components/EmptyState.jsx';
import { Spinner } from '../../components/Spinner.jsx';
import { IosSearchField } from '../../components/IosControls.jsx';
import {
  disableTestStore,
  fetchLibrary,
  fetchStoreCards,
  fetchTestStoreState,
  forceRemoveModel,
  startDeploy,
  startDownload,
  startEnable,
  startStop,
  startUninstall,
  storeAvailability,
  takeStartupCleanup,
  unlockTestStore,
} from './model-store-api.js';
import {
  advanceUnlockClicks,
  cardIdFromLifecycleKey,
  downloadTaskKey,
  filterModels,
  formatBytes,
  lifecycleTaskKey,
  mergeModels,
  sortModels,
  storeGuideState,
  uninstallDoneText,
  uninstallOptionsFor,
} from './model-store-logic.js';
import { tryGetTauriBridge } from '../../platform/tauri/client.js';
import { findLinkedRows } from './pinvou-model-link.js';
// 发起/取消/应答/关闭都是模块级函数,身份恒定。直接引用而不是从 hook 的返回值上
// 取,这样下面那些 useCallback 的依赖数组才是稳的——挂在 hook 返回对象上的话,
// 每来一批日志行都会让它们全部重建。
import {
  answerTask,
  cancelTask,
  dismissTask,
  reattachActiveRuns,
  runTask,
  subscribeTaskSettled,
  toggleTaskExpand,
  useLmmTasks,
} from './useLmmTasks.js';
import { StoreGuide } from './ModelStoreCards.jsx';
import { ModelRow } from './ModelRow.jsx';
import { ModelStoreTaskPanel } from './ModelStoreTaskPanel.jsx';
import { ModelDetailDialog } from './ModelDetailDialog.jsx';
import { StoreCardDialog } from './StoreCardDialog.jsx';
import { StorageSettingsDialog } from './StorageSettingsDialog.jsx';
import {
  BTN_NEUTRAL,
  HOVER_SURFACE,
  INPUT_CLASS,
  MsButton,
  MsDialog,
  MsDialogTitle,
  MsToast,
  TEXT_PRIMARY,
  TEXT_SECONDARY,
} from './model-store-ui.jsx';

const TOAST_MS = 3200;

// 顶部那一行胶囊按钮的规格。筛选和数据源切换共用 —— 它们在同一行里并排出现,
// 差一档字重或一像素高度都会被看出来。
// 口令框刻意不给关闭回调:见下面用到它的地方。提到模块级是为了保持引用稳定,
// 否则 MsDialog 里那个按 Escape 的监听会跟着每次重渲染反复装卸。
const NOOP = () => {};

const PILL_BASE = 'inline-flex h-9 items-center rounded-full px-3.5 text-[13px] font-semibold transition-colors';
const PILL_ON = 'bg-[#3A3A3C] text-white dark:bg-white dark:text-black';
const PILL_OFF = 'bg-[#F2F2F7] text-black hover:bg-[#E5E5EA] dark:bg-[#2C2C2E] dark:text-white dark:hover:bg-[#3A3A3C]';
const EMPTY_LIBRARY = { models: [], corrected: [] };

const ModelStoreView = ({ theme, t, bs }) => {
  const copy = t.uiModelStore;
  // 桥接快照只做「宿主是否已就绪」的标记;模型商店的数据走 LMM 自己的管理接口,
  // 不从 bs 里取。
  const bridgeReady = !!bs;

  const [status, setStatus] = useState(null);

  // 原来是 tab（模型市场 / 我的模型），现在合成一份列表用 filter 筛。
  // 两个 Tab 是实现视角的产物（商店来自远端目录、本地库来自 sqlite），
  // 用户视角里它们是同一件事，见 model-store-logic.js 的 mergeModels。
  const [filter, setFilter] = useState('all');
  const [query, setQuery] = useState('');
  // 商店卡片和本地库是**两份独立的数据、两个独立的错误态**。
  //
  // 它们原来绑在一个 Promise.all 上:商店端点抖一下,整个页面就换成错误占位,
  // 本地已装模型的详情/启用/停用/卸载全部够不着,只剩一个会把同一个组合请求
  // 再发一遍的重试按钮。而本地库查询是纯本进程的 sqlite + 容器状态查询,
  // 它没有理由跟着远端商店一起失败。
  const [cards, setCards] = useState(null);
  const [library, setLibrary] = useState(EMPTY_LIBRARY);
  const [storeError, setStoreError] = useState(null);
  const [libraryError, setLibraryError] = useState(null);
  const [storeLoading, setStoreLoading] = useState(false);
  const [libraryLoading, setLibraryLoading] = useState(false);
  const loading = storeLoading || libraryLoading;

  const [detailCard, setDetailCard] = useState(null);
  const [detailCardId, setDetailCardId] = useState(null);
  const [confirmUninstall, setConfirmUninstall] = useState(null);
  // 卸载失败后的逃生路径。两段:'confirm' 是二次确认,'result' 是移除之后把
  // LMM 报回来的残留路径摊开给用户看。
  //
  // 为什么结果要单独一段而不是只弹一条 toast:残留路径是**做完之后**才知道的
  // (LMM 按卡片脚本的约定反推候选路径、再用 exists() 过滤,做之前问不出来),
  // 而这几行路径就是用户之后唯一能凭以去清磁盘的线索。塞进一个 3.2 秒就消失的
  // 单行胶囊里等于没给。toast 照发,但只报个结论。
  const [forceRemove, setForceRemove] = useState(null);
  const [forceRemoveBusy, setForceRemoveBusy] = useState(false);
  const [profilePick, setProfilePick] = useState(null);
  const [toast, setToast] = useState(null);
  // 「存储与导入」设置框。权重目录跟用户实际存放位置对不上时，下载会白跑几十
  // 个小时而界面上看不出异常,所以这个入口是常驻的,不藏。
  const [storageOpen, setStorageOpen] = useState(false);

  // 测试模型商店:卡片和脚本打包在 LMM 内、不依赖商店服务器,只给开发验证脚本
  // 本身用。所以它默认关着,而且开关是**隐藏**的 —— 见下面 handleSecretUnlockClick。
  const [testStoreEnabled, setTestStoreEnabled] = useState(false);
  // 数据源。开启测试商店只是多一个数据源,界面还是同一套。
  const [source, setSource] = useState('official');
  const [unlockOpen, setUnlockOpen] = useState(false);
  const [unlockInput, setUnlockInput] = useState('');
  // 连点计数放 ref 不放 state:它不该触发重渲染,而且必须能在同一次事件序列里
  // 立刻读到最新值 —— 用 state 的话闭包里拿到的是上一次的旧值。
  const unlockClicksRef = useRef(null);

  const testMode = source === 'test';

  const { tasks, runningTask } = useLmmTasks();
  const toastTimerRef = useRef(null);
  // 当前有效的那次加载。见 loadCards / loadLibrary —— 并发加载时用它丢掉过期响应。
  // 两份数据各自一个序号:它们现在是分开发的请求,共用一个序号会让快的那个把慢的
  // 那个判成过期。
  const cardsTokenRef = useRef(0);
  const libraryTokenRef = useRef(0);

  const showToast = useCallback((text) => {
    setToast({ text });
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
    toastTimerRef.current = setTimeout(() => setToast(null), TOAST_MS);
  }, []);

  useEffect(() => () => {
    if (toastTimerRef.current) clearTimeout(toastTimerRef.current);
  }, []);

  const refreshStatus = useCallback(async () => {
    try {
      setStatus(await storeAvailability());
    } catch (error) {
      // 连可用性都问不到,只可能是命令层没注册(构建里缺了这个模块)。按「这个平台
      // 没有」处理,并把原始错误当成原因显示出来——它是唯一的线索。
      setStatus({ available: false, reason: String(error?.message || error) });
    }
  }, []);

  // 加载商店在架卡片。
  //
  // 序号那道闸门:慢的旧响应落地时能认出自己已经过期,直接丢掉。没有它的话,
  // 切数据源胶囊(testMode 变化 → effect 重跑)、加载中点刷新、后台任务完成触发的
  // 刷新跟用户操作撞在一起,都会产生并发请求;先发的那个若后回来,就会把新数据
  // 覆盖成旧数据——界面显示的卡片跟当前数据源对不上,而转圈早被先落地的那个关掉了。
  // 同 PR 的 ModelDetailDialog 用的是同一个套路。
  const loadCards = useCallback(async () => {
    const token = (cardsTokenRef.current += 1);
    const stale = () => token !== cardsTokenRef.current;
    setStoreLoading(true);
    try {
      const cardList = await fetchStoreCards(testMode);
      if (stale()) return;
      setCards(Array.isArray(cardList) ? cardList : []);
      setStoreError(null);
    } catch (error) {
      // 过期请求的失败也要丢掉:否则切到测试商店之后,正式商店那次的超时
      // 会把已经加载好的测试商店列表打成错误态。
      if (!stale()) setStoreError(String(error?.message || error));
    } finally {
      if (!stale()) setStoreLoading(false);
    }
  }, [testMode]);

  /** 加载本地模型库。纯本进程查询,不该跟着远端商店一起失败,所以单独一条路。 */
  const loadLibrary = useCallback(async () => {
    const token = (libraryTokenRef.current += 1);
    const stale = () => token !== libraryTokenRef.current;
    setLibraryLoading(true);
    try {
      const libraryList = await fetchLibrary();
      if (stale()) return;
      setLibrary(libraryList || EMPTY_LIBRARY);
      setLibraryError(null);
      // 启动时按实际容器状态纠正过运行标记的模型:用户上次看到的「运行中」其实
      // 已经不在了,不说一声他会以为模型还在服务。
      const corrected = libraryList?.corrected ?? [];
      if (corrected.length > 0) showToast(copy.corrected(corrected[0]));
    } catch (error) {
      if (!stale()) setLibraryError(String(error?.message || error));
    } finally {
      if (!stale()) setLibraryLoading(false);
    }
  }, [copy, showToast]);

  /**
   * 两份数据一起刷。**刻意不是 Promise.all**:两条路各自成败、各自记错误态,
   * 一边挂了不能把另一边的功能一起拖下水。
   * @returns {Promise<void>} 两边都落地后 resolve(各自已经吞掉自己的错误)。
   */
  const loadAll = useCallback(async () => {
    await Promise.all([loadCards(), loadLibrary()]);
  }, [loadCards, loadLibrary]);

  const guide = storeGuideState(status);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- 进入页面时问一次可用性;它由后端按编译目标决定,只能这样同步进来
    refreshStatus();
  }, [refreshStatus]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- 确认这个平台有模型商店之后才去拉商店和本地库
    if (guide === 'ready') loadAll();
  }, [guide, loadAll]);

  // 启动清理的结果：上次应用被强杀/崩溃后残留的脚本进程，这一轮启动时收掉了哪些。
  //
  // **必须真的来取。** 后端收完就把报告攒在内存里等人取（取一次就没了），没人取
  // 就等于没做——而它要告诉用户的正是那起事故：残留的下载进程占着权重目录的文件
  // 锁，把下一次下载堵了 16 小时，期间界面上一点提示都没有。
  //
  // root 身份跑的那些收不掉（普通用户没权限），更要说出来：用户得自己去处理，
  // 否则下次下载还是会被堵住，而且他不会知道为什么。
  useEffect(() => {
    if (guide !== 'ready') return undefined;
    let alive = true;
    takeStartupCleanup()
      .then((report) => {
        if (!alive || !report) return;
        const killed = report.killed?.length ?? 0;
        const stuck = report.skipped_root?.length ?? 0;
        if (stuck > 0) showToast(copy.startupCleanupStuck(stuck));
        else if (killed > 0) showToast(copy.startupCleanupKilled(killed));
      })
      // 取不到不打扰用户：这只是一条事后提示，不该因为它冒出一个错误框。
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [copy, guide, showToast]);

  // 进页面读一次测试商店开关。sidecar 没起来时不问 —— 那只会拿到一个连接错误。
  //
  // 拿不到就当它关着:老版本 LMM 没有这条接口,而"问不出来"和"没开"在界面上
  // 应该是同一个样子 —— 一点痕迹都不留。
  useEffect(() => {
    if (guide !== 'ready') return undefined;
    let cancelled = false;
    fetchTestStoreState()
      .then((state) => {
        // eslint-disable-next-line react-hooks/set-state-in-effect -- 开关存在 LMM 那边(另一个进程),只能这样同步进来
        if (!cancelled) setTestStoreEnabled(!!state?.enabled);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [guide]);

  // 隐藏入口:连点页面标题 7 次(间隔不超过 1.5 秒)才弹口令框,见 advanceUnlockClicks。
  //
  // 全程不给任何提示 —— 提示等于把入口告诉了所有人,那这个入口就白藏了。
  const handleSecretUnlockClick = useCallback(() => {
    if (testStoreEnabled) return; // 已经开着,不用再点
    const next = advanceUnlockClicks(unlockClicksRef.current, Date.now());
    unlockClicksRef.current = next;
    if (next.reached) setUnlockOpen(true);
  }, [testStoreEnabled]);

  const closeUnlock = useCallback(() => {
    setUnlockOpen(false);
    setUnlockInput('');
  }, []);

  const submitUnlock = useCallback(async () => {
    try {
      const result = await unlockTestStore(unlockInput);
      // 口令错回的是 ok:false 而不是 HTTP 错误,所以走的是这条正常路径:只提示
      // 一句、把框留着让用户重输,而不是当成故障把框关掉。
      if (!result?.ok) {
        showToast(copy.testStoreWrongPassword);
        return;
      }
      closeUnlock();
      setTestStoreEnabled(true);
      showToast(copy.testStoreUnlocked);
    } catch (error) {
      showToast(String(error?.message || error));
    }
  }, [closeUnlock, copy, showToast, unlockInput]);

  const handleDisableTestStore = useCallback(async () => {
    try {
      await disableTestStore();
      setTestStoreEnabled(false);
      // 数据源切回正式商店:测试卡片这时已经拉不到了,留在测试源上只会得到一片空。
      setSource('official');
      showToast(copy.testStoreDisabled);
    } catch (error) {
      showToast(String(error?.message || error));
    }
  }, [copy, showToast]);

  // 这里原来有一整套「拉起 sidecar」的流程：handleStart、starting/startError 两个
  // 状态、一个"只自动试一次"的 autoStartedRef，以及引导区那个「启动」按钮。
  //
  // 模型商店改成原生实现之后它们全部消失了——没有另一个进程需要拉起，代码就在
  // 本进程里。可用性由编译目标决定（Linux 有、其余没有），不存在"启动失败"这种
  // 中间态，也就没有什么可以让用户重试。

  // 合并 → 筛选 → 排序。三步分开是为了各自可测（见 tests/model_store_merge.test.mjs）。
  // 「已下载」以本地库记录为准：取消/失败的下载不会写库，也就不会误标记成已有。
  const mergedModels = useMemo(() => mergeModels(cards, library), [cards, library]);
  const visibleModels = useMemo(
    () => sortModels(filterModels(mergedModels, { query, filter })),
    [mergedModels, query, filter]
  );

  /**
   * 后端报回来的一次活跃运行,在界面上该叫什么、算哪个 key。
   *
   * 后端只给 `card_id` 和机器名 `kind`——它不该知道界面怎么措辞,更不该往 IPC 里
   * 塞一句中文。模型名在合并后的列表里查;查不到就退回用 card_id 兜底,不能因此
   * 把这次运行丢掉(库还没加载完、或者卡片刚被删,恰恰是最需要看到这张任务卡的
   * 时刻)。
   * @param {{card_id?:string, kind?:string}} run - 后端给的活跃运行。
   * @returns {{title:string, key:string}|null} 标题与任务 key;认不出的操作返回 null。
   */
  const describeActiveRun = useCallback(
    (run) => {
      const cardId = run?.card_id;
      if (!cardId) return null;
      const model = mergedModels.find((item) => item.id === cardId) || null;
      const name = model?.name || model?.record?.card_name || cardId;
      switch (run.kind) {
        case 'download':
          return { title: copy.taskDownload(name), key: downloadTaskKey(cardId) };
        case 'deploy':
          return { title: copy.taskDeploy(name), key: lifecycleTaskKey(cardId) };
        case 'enable':
          return { title: copy.taskEnable(name), key: lifecycleTaskKey(cardId) };
        case 'stop':
          return { title: copy.taskStop(name), key: lifecycleTaskKey(cardId) };
        case 'uninstall':
          return { title: copy.taskUninstall(name), key: lifecycleTaskKey(cardId) };
        // 认不出的操作不接:宁可不显示,也不要一张说不清自己在干什么、却点得了
        // 取消的任务卡。卡片脚本和应用可以不同版本,这条兜底不能省。
        default:
          return null;
      }
    },
    [copy, mergedModels]
  );

  // 再附着:把后端还在跑的长操作接回任务面板。
  //
  // 这是本视图**条件挂载**带来的必修课(main.jsx 里 `currentView === 'modelStore' &&
  // <ModelStoreView/>`):切一下视图整棵树就卸载。任务表本身已经提到模块级、不随
  // 卸载消失,但还有两种真的会失联——页面整个重载(开发期热更、崩溃恢复),以及
  // 轮询连续失败后前端自己放弃而后端其实还在跑。接不回来的后果很实在:几十 GB
  // 的下载没有进度、点不了取消、脚本请求输入也没人答(600 秒后被判死),再点一次
  // 下载还会撞上后端的卡片互斥,被渲染成一张说不清原因的失败卡。
  //
  // 跟着 describeActiveRun 走(它依赖合并后的列表),所以每次刷新完列表都会顺手
  // 再问一次。已经挂着轮询的运行会被跳过,重复问是安全的。
  useEffect(() => {
    if (guide === 'ready') reattachActiveRuns(describeActiveRun);
  }, [describeActiveRun, guide]);

  // 接回来的任务没有等在 await 上的调用方(它是上一次挂载、甚至上一次页面加载
  // 发起的),跑完之后得有人去刷模型库——否则任务卡消失了,列表还停在「下载中」
  // 的那一版。自己发起的那些由各自的 handler 收尾,这里不重复刷。
  useEffect(() => subscribeTaskSettled((task) => {
    if (task?.reattached) loadLibrary();
  }), [loadLibrary]);

  /**
   * 把品悟模型列表里指向某个已消失模型的行清掉。
   *
   * 正常卸载和强制移除**是同一个后果**：LMM 侧的库记录一没，网关对这个 model_name
   * 就一律回 404「未找到模型」。而 404 在多数 OpenAI 客户端眼里是"地址写错了"，
   * 不是"模型没了"；接入时又会顺手把它设为当前模型，所以这行死数据很可能正是
   * 默认模型，用户下一次新建会话第一句话就失败，还找不到回模型商店的路。
   *
   * `quiet` 给强制移除用：那条路径的结果页已经在说"磁盘上还有残留"，再叠一句
   * "顺带清了几行配置"只会稀释更要紧的提醒。但**清不掉时无论如何都要说**——
   * 品悟不允许删掉最后一个模型，那种情况下用户会留着一个静默失效的默认模型。
   * @param {string|undefined} modelName - LMM 的 model_name。
   * @param {string} displayName - 用于提示的模型名。
   * @param {{quiet?: boolean}} [options] - 成功时要不要出声。
   * @returns {Promise<boolean>} 清干净了(或本来就没什么要清)为 true;清不掉为 false。
   */
  const cleanLinkedRows = useCallback(
    // names 要把这个模型的**全部标识**都带上（model_name 和 endpoint_model）。
    // 接入时存的是 endpoint_model，只按 model_name 找必然落空——见 findLinkedRows。
    async (names, displayName, { quiet = false } = {}) => {
      const bridge = tryGetTauriBridge();
      // **先分清「查过了没有」和「根本没查成」**。
      //
      // `findLinkedRows` 对 `undefined` 和空数组返回的都是 `[]`，下面那句
      // `rows.length === 0 → return true` 于是会在**我们压根没能力查看**的情况下
      // 宣告清理成功。品悟模型列表那一行照样留着，而调用方拿到 true、提示语说
      // 「已卸载」，用户毫不知情——2026-09-22 真机上驭光卡删除后留下的那行死数据
      // 就是这么来的（卸载本身成功、清理这步悄悄放过）。
      //
      // 拿不到快照时返回 false：宁可让用户看见一句「没能自动清掉，请手动删」，
      // 也不能让他以为清干净了。
      if (!Array.isArray(bs?.savedModels)) {
        showToast(copy.linkBridgeMissing);
        return false;
      }
      const rows = findLinkedRows(bs.savedModels, ...names);
      // 真的没有死行要清:这是最常见的情况(用户从没接入过这个模型),不必出声。
      if (rows.length === 0) return true;
      // 有死行、但拿不到 deleteModel。这两个条件**不能**合并成一句 return:合并之后
      // 「该清但清不掉」会被当成「没什么要清的」静默放过,留下一行静默失效的默认模型。
      // linkBridgeMissing 是一条**定值文案**(不是模板函数),别当函数调用。
      if (!bridge?.models?.deleteModel) {
        showToast(copy.linkBridgeMissing);
        return false;
      }
      for (const row of rows) {
        try {
          await bridge.models.deleteModel(row.id);
        } catch (error) {
          showToast(copy.linkRowStale(row.name || displayName, String(error?.message || error)));
          // 清不掉是**失败**,要让调用方知道,否则它会紧接着弹一句成功提示把这条盖掉。
          return false;
        }
      }
      if (!quiet) showToast(copy.linkRowRemoved(displayName, rows.length));
      return true;
    },
    [bs, copy, showToast]
  );

  const handleDownload = useCallback(
    async (card) => {
      const result = await runTask({
        title: copy.taskDownload(card.name),
        kind: 'download',
        key: downloadTaskKey(card.id),
        start: () => startDownload(card.id, testMode),
      });
      if (result.duplicate || result.aborted) return;
      if (result.ok) showToast(copy.downloadDone(card.name));
      // 失败详情留在任务面板里(含日志),这里不再重复一条 toast;但库照样要刷新,
      // 中断的下载可能已经改变了本地状态。
      loadAll();
    },
    [copy, loadAll, showToast, testMode]
  );

  // `doneText` 可以是一句现成的话，也可以是 `(outcome) => string`：卸载要按服务端
  // 回的实际结果措辞，而那份结果在发请求时还不存在。
  const runLifecycle = useCallback(
    async ({ record, kind, title, start, doneText }) => {
      const result = await runTask({ title, kind, key: lifecycleTaskKey(record.card_id), start });
      if (result.duplicate || result.aborted) return result;
      if (result.ok) showToast(typeof doneText === 'function' ? doneText(result.result) : doneText);
      loadAll();
      // 把结果交回调用方：卸载成功后还要顺带清理品悟模型列表里的死行。
      return result;
    },
    [loadAll, showToast]
  );

  const deployWithProfile = useCallback(
    (record, profileId) => runLifecycle({
      record,
      kind: 'deploy',
      title: copy.taskDeploy(record.card_name),
      start: () => startDeploy(record.card_id, profileId),
      doneText: copy.deployDone(record.card_name),
    }),
    [copy, runLifecycle]
  );

  const handleDeploy = useCallback(
    (record) => {
      // 多套部署方案的卡片先弹方案选择器;单方案/无方案卡片直接部署。
      const profiles = record.profiles || [];
      if (profiles.length > 1) {
        setProfilePick({ record, selectedId: record.active_profile_id || profiles[0].id });
        return;
      }
      deployWithProfile(record, null);
    },
    [deployWithProfile]
  );

  const handleEnable = useCallback(
    (record) => runLifecycle({
      record,
      kind: 'enable',
      title: copy.taskEnable(record.card_name),
      start: () => startEnable(record.card_id),
      doneText: copy.enableDone(record.card_name),
    }),
    [copy, runLifecycle]
  );

  const handleStop = useCallback(
    (record) => runLifecycle({
      record,
      kind: 'stop',
      title: copy.taskStop(record.card_name),
      start: () => startStop(record.card_id),
      doneText: copy.stopDone(record.card_name),
    }),
    [copy, runLifecycle]
  );

  /**
   * 卸载 / 删除。`purgeWeights` 就是用户在对话框上按的那个按钮：
   * false = 卸载（保留权重，卡片留在库里退回「已下载」），
   * true  = 删除（连权重一起清掉，卡片移出模型库）。
   *
   * 两个按钮都走这里，但**后果不再相同**：卸载之后卡片还在列表里（`loadAll()`
   * 会把它刷成「已下载」），删除之后整行消失。
   *
   * `cleanLinkedRows` 两边都要跑：**判据是"这个模型还能不能被调用"，不是"记录
   * 还在不在"**。卸载之后服务和容器都没了，品悟模型列表里那一行照样是死的——
   * 而它很可能正是当前默认模型。
   *
   * 完成提示按**服务端回的实际结果**说，不是按用户点了哪个按钮说。
   *
   * uninstall_payload 里有 purged_weights / freed_bytes / remaining_paths，就是为了
   * 这件事：卸载脚本退出 0 但权重没删干净是真实发生过的（设备上 21 GB 的
   * Qwen3.8-27B-NVFP4 就是这么留下的）。那种情况下按请求参数拼话会说出「模型权重
   * 已删除」——而磁盘一个字节都没少，卡片又已经从列表消失，此后没有任何界面能管到
   * 那堆文件。所以 remaining_paths 非空时必须改口，不能替脚本把话圆过去。
   * @param {boolean} purgeWeights - true = 连模型权重一起删除。
   * @returns {Promise<void>} 无返回。
   */
  const handleUninstall = useCallback(async (purgeWeights) => {
    const record = confirmUninstall;
    setConfirmUninstall(null);
    const result = await runLifecycle({
      record,
      kind: 'uninstall',
      title: copy.taskUninstall(record.card_name),
      start: () => startUninstall(record.card_id, purgeWeights),
      // doneText 交给 runLifecycle 从 run 结果里现算：请求发出时还不知道脚本删掉了什么。
      doneText: (outcome) => uninstallDoneText(copy, record.card_name, outcome),
    });
    if (!result?.ok) return;
    // 卸载成功后清掉品悟模型列表里指向它的那些行（理由见 cleanLinkedRows 的注释）。
    // 这条路径出声：用户主动卸载了一个模型，顺带清掉了几行配置，该让他知道。
    //
    // **返回值必须接住**。这里原先是裸 `await`，于是清理失败被整个吞掉：卸载本身
    // 的成功提示照发，用户看到「已卸载并删除」就走了，而品悟模型列表里那一行还
    // 指着一个已经不存在的本地服务——下一次新建会话第一句话就失败，且那个报错里
    // 找不到任何通往模型商店的路。真机上就是这么留下一行的（2026-09-22 驭光卡）。
    //
    // 强制移除那条路径一直是接住的（`const linksCleaned = ...`），两条路不该有
    // 差别：判据是「这个模型还能不能被调用」，跟用户点的是哪个按钮无关。
    const linksCleaned = await cleanLinkedRows(
      [record.endpoint_model, record.model_name],
      record.card_name,
    );
    if (!linksCleaned) showToast(copy.uninstallLinksLeft(record.card_name));
  }, [cleanLinkedRows, confirmUninstall, copy, runLifecycle, showToast]);

  /**
   * 点右侧那个删除图标的入口。
   *
   * 运行中的模型**不直接进卸载对话框**，先让用户停用。后端也会拒（见
   * `lifecycle.rs` 的 `uninstall`），这里只是把同一条规则提前告诉用户，并且
   * 给出下一步——直接把按钮禁掉的话，用户只知道点不动，不知道该干什么。
   *
   * 挡的不是误点，是「服务没停干净就删掉权重和库记录」之后留下的无主进程：
   * 记录一删，判断容器归属的 `container_name` 也没了，端口和显存占着而界面上
   * 再也停不掉它。
   *
   * @param {object} record - 库记录。
   * @returns {void} 无返回。
   */
  const requestUninstall = useCallback((record) => {
    if (record?.running) {
      showToast(copy.uninstallStopFirst(record.card_name));
      return;
    }
    setConfirmUninstall(record);
  }, [copy, showToast]);

  // 卸载失败 → 任务卡上那个「强制从列表移除」的入口点进来。
  //
  // 任务对象里只有 key(`card:<卡片id>`),没有卡片名和体积,所以要回合并后的列表
  // 里查一次 —— 确认对话框必须报出用户认得的那个名字,光给一个卡片 ID 他没法
  // 判断自己要删的是不是这张。查不到就退回用 ID 兜底,不能因此把入口废掉:
  // 记录被别处删掉了、列表还没刷新,恰恰是最需要这条逃生路径的时刻。
  const requestForceRemove = useCallback(
    (task) => {
      const cardId = cardIdFromLifecycleKey(task?.key);
      if (!cardId) return;
      const model = mergedModels.find((item) => item.id === cardId) || null;
      setForceRemove({
        stage: 'confirm',
        taskId: task.id,
        cardId,
        cardName: model?.record?.card_name || model?.name || cardId,
        sizeBytes: model?.size_bytes ?? model?.record?.size_bytes ?? 0,
        result: null,
      });
    },
    [mergedModels]
  );

  const closeForceRemove = useCallback(() => setForceRemove(null), []);

  const confirmForceRemove = useCallback(async () => {
    const target = forceRemove;
    if (!target) return;
    setForceRemoveBusy(true);
    try {
      const result = await forceRemoveModel(target.cardId);
      // 那张失败的任务卡该走了:它指向的记录已经不存在,留着只会让用户再点一次
      // 同一个入口,然后收到一句「没有这张卡」。
      dismissTask(target.taskId);
      // 不 await:列表刷新是背景动作,结果页该立刻出来。
      loadAll();
      // 品悟模型列表里指向这个模型的行也要一起清掉——**跟正常卸载是同一个后果**。
      // 记录一没,网关对这个 model_name 一律回 404「未找到模型」,而 404 在多数
      // 客户端眼里是"地址写错了"而不是"模型没了"。接入时还会顺手把它设为当前模型,
      // 所以这行死数据很可能正是默认模型,用户下一次新建会话第一句话就失败。
      //
      // 这里刻意不弹第二条 toast:强制移除的结果页已经在说"磁盘上还有残留"了,
      // 再叠一句"顺带清了几行配置"只会稀释那个更要紧的提醒。清不掉时才说话。
      const linksCleaned = await cleanLinkedRows(
        [result?.endpoint_model, result?.model_name],
        target.cardName,
        { quiet: true },
      );
      const residual = Array.isArray(result?.residual_paths) ? result.residual_paths : [];
      // toast 只有一个槽位,后一条会把前一条顶掉。cleanLinkedRows 清不掉时已经弹了
      // 一条「请到设置里手动删除」——那是这条路径上唯一的提醒,再补一句"移除完成"
      // 会在同一 tick 把它盖掉,用户永远看不到。残留信息结果页上本来就有,让位给
      // 那条更要紧的。
      if (linksCleaned) showToast(copy.forceRemoveDone(target.cardName, residual.length));
      setForceRemove({ ...target, stage: 'result', result: { ...result, residual_paths: residual } });
    } catch (error) {
      // 失败就把框关掉、只留一句话:记录还在,那张任务卡也还在,用户可以再试。
      setForceRemove(null);
      showToast(copy.forceRemoveFailed(String(error?.message || error)));
    } finally {
      setForceRemoveBusy(false);
    }
    // cleanLinkedRows 必须在依赖里。它依赖 bs（品悟的模型列表快照），漏掉的话
    // bs 一变它就重建、而这里不重建，强制移除会拿着旧快照去 findLinkedRows：
    // 要么漏清刚接入的那几行，要么对着已经不存在的 id 调 deleteModel、把一次
    // 成功清理报成失败。残留路径结果页是用户清磁盘的唯一凭据,不能被闭包竞态吞掉。
  }, [cleanLinkedRows, copy, forceRemove, loadAll, showToast]);

  const confirmProfileDeploy = useCallback(() => {
    const { record, selectedId } = profilePick;
    setProfilePick(null);
    deployWithProfile(record, selectedId);
  }, [deployWithProfile, profilePick]);

  /** 筛选胶囊。「已安装」= 本机有权重的（下载过就算），这是用户最常问的那个问题。 */
  const FILTERS = [
    { key: 'all', label: copy.filterAll },
    { key: 'installed', label: copy.filterInstalled },
    { key: 'running', label: copy.filterRunning },
  ];

  /** 数据源切换。只在测试商店开着时出现;界面是同一套,换的只是卡片从哪来。 */
  const SOURCES = [
    { key: 'official', label: copy.sourceOfficial },
    { key: 'test', label: copy.sourceTest },
  ];

  /**
   * 打开详情。已下载的走库详情（有运行态、日志、Key），没下载的走商店卡片详情。
   * 两个弹窗承载的信息不一样，不能合并——库详情要的字段商店卡片根本没有。
   * @param {object} model - 合并后的模型项。
   */
  const openDetail = (model) => {
    if (model.record) setDetailCardId(model.id);
    else setDetailCard(model);
  };

  /**
   * 一个分区：标题 + 计数 + 两列行卡。
   * @param {{titleKey: string, items: Array<object>, testId: string}} params - 参数。
   * @returns {React.ReactElement|null} 分区，空则不渲染。
   */
  const renderSection = ({ titleKey, items, testId }) => {
    if (items.length === 0) return null;
    return (
      <section className="mb-8" data-testid={testId}>
        <h3 className="mb-2 px-3 text-[13px] font-bold uppercase tracking-wider text-[#3C3C43]/60 dark:text-[#EBEBF5]/60">
          {copy[titleKey]}
          <span className="ml-1.5 font-semibold opacity-60">{items.length}</span>
        </h3>
        <div className="grid grid-cols-1 gap-x-6 lg:grid-cols-2">
          {items.map((model) => (
            <ModelRow
              key={model.id}
              model={model}
              copy={copy}
              busyKind={
                model.state === 'available'
                  ? (runningTask(downloadTaskKey(model.id))?.kind || null)
                  : (runningTask(lifecycleTaskKey(model.id))?.kind || null)
              }
              onOpen={openDetail}
              onDownload={handleDownload}
              onDeploy={handleDeploy}
              onEnable={handleEnable}
              onStop={handleStop}
              onUninstall={requestUninstall}
            />
          ))}
        </div>
      </section>
    );
  };

  const renderModels = () => {
    if (cards === null && (library.models || []).length === 0) return null;
    if (visibleModels.length === 0) {
      return (
        <EmptyState
          testId="model-store-empty"
          className="flex flex-col items-center py-24"
          icon={<Package size={28} />}
          iconClassName="mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-slate-100 text-slate-400 dark:bg-slate-800"
          title={query.trim() ? copy.storeSearchEmpty : copy.storeEmpty}
          titleClassName="mb-2 text-xl font-semibold text-slate-800 dark:text-slate-200"
          hintClassName="text-slate-500 dark:text-slate-400"
        />
      );
    }
    const installed = visibleModels.filter((model) => model.state !== 'available');
    const available = visibleModels.filter((model) => model.state === 'available');
    return (
      <div data-testid="model-store-list">
        {renderSection({ titleKey: 'sectionInstalled', items: installed, testId: 'model-store-section-installed' })}
        {/* 一台新设备上「本机已有」是空的，这时候要说清楚下一步做什么，
            而不是只让用户看见一个「可下载」分区自己去猜。 */}
        {installed.length === 0 && filter === 'all' && !query.trim() ? (
          <div className="mb-8 px-3">
            <div className="text-[15px] font-semibold text-slate-800 dark:text-slate-200">{copy.emptyInstalled}</div>
            <div className="mt-1 text-[13px] text-slate-500 dark:text-slate-400">{copy.emptyInstalledHint}</div>
          </div>
        ) : null}
        {renderSection({ titleKey: 'sectionAvailable', items: available, testId: 'model-store-section-available' })}
      </div>
    );
  };

  /**
   * 一条「这半边加载失败了」的横幅。
   *
   * 商店挂了不能把本地库一起换成错误占位:本地已装模型的详情/启用/停用/卸载都还
   * 好好的,把整页换掉等于连它们一起停掉。所以失败的那一半只在列表上方说一句,
   * 并给自己那一条的重试,另一半照常可用。
   * @param {{testId:string, text:string, onRetry:()=>void}} params - 参数。
   * @returns {React.ReactElement} 横幅。
   */
  const renderLoadError = ({ testId, text, onRetry }) => (
    <div
      data-testid={testId}
      className="mb-4 mx-3 flex flex-wrap items-center justify-between gap-2 rounded-[14px] bg-rose-50 px-3 py-2 dark:bg-rose-500/10"
    >
      <span className="flex min-w-0 items-center gap-2 text-[12px] text-rose-700 dark:text-rose-300">
        <AlertTriangle size={14} className="shrink-0 opacity-80" />
        <span className="min-w-0 break-words">{text}</span>
      </span>
      <MsButton tone="ghost" onClick={onRetry}>{copy.retry}</MsButton>
    </div>
  );

  const renderBody = () => {
    if (guide !== 'ready') {
      return (
        <StoreGuide state={guide} copy={copy} theme={theme} reason={status?.reason || null} />
      );
    }
    // 两边都挂了才整页换成错误态——那时候确实一个模型都摆不出来。
    if (storeError && libraryError) {
      return (
        <EmptyState
          testId="model-store-error"
          className="flex flex-col items-center py-24"
          icon={<AlertTriangle size={28} />}
          iconClassName="mb-4 flex h-16 w-16 items-center justify-center rounded-full bg-rose-50 text-rose-500 dark:bg-rose-500/10"
          title={copy.loadFailed(libraryError)}
          titleClassName="mb-2 max-w-[520px] text-[15px] text-slate-600 dark:text-slate-300"
          action={
            <button type="button" className={BTN_NEUTRAL} onClick={loadAll}>{copy.retry}</button>
          }
        />
      );
    }
    if (loading && cards === null && (library.models || []).length === 0) {
      return (
        <EmptyState
          testId="model-store-loading"
          className="flex flex-col items-center py-24"
          icon={<Spinner size={24} variant="top" tone="brand" />}
          iconClassName="mb-4"
          title={copy.loading}
          titleClassName="text-[15px] text-slate-500 dark:text-slate-400"
        />
      );
    }
    return (
      <>
        {storeError
          ? renderLoadError({
            testId: 'model-store-cards-error',
            text: copy.storeLoadFailed(storeError),
            onRetry: loadCards,
          })
          : null}
        {libraryError
          ? renderLoadError({
            testId: 'model-store-library-error',
            text: copy.libraryLoadFailed(libraryError),
            onRetry: loadLibrary,
          })
          : null}
        {renderModels()}
      </>
    );
  };

  return (
    <div
      data-testid="model-store-view"
      data-bridge-ready={bridgeReady ? 'true' : 'false'}
      data-lmm-state={guide}
      className="flex min-w-0 flex-1 flex-col overflow-y-auto bg-white p-4 font-sans text-slate-900 transition-colors duration-300 custom-scrollbar sm:p-6 lg:p-10 dark:bg-[#131314] dark:text-white"
    >
      {/* 骨架跟插件中心逐字对齐（features/tools/ToolStoreView.jsx）：同样的
          p-4 sm:p-6 lg:p-10 + max-w-[1400px] + 标题区 border-b。用户会把两个
          商店放在一起看，这里差一点都显眼。 */}
      <header className="z-30 bg-white/80 backdrop-blur-2xl dark:bg-[#131314]/80">
        <div className="mx-auto flex max-w-[1400px] flex-wrap items-center gap-3 border-b border-slate-200/50 pb-6 dark:border-white/10">
          {/* 标题就是测试商店的隐藏入口：连点 7 次（间隔不超过 1.5 秒）才弹口令框。
              原来挂在列表底部那行「本地模型管理 vX.Y.Z」上，但模型商店并入品悟之后
              不该再有自己的版本号——它是品悟的一部分，不是一个搭在里面的独立应用。
              标题是页面上唯一一直都在、又不承担任何点击语义的元素，挪到这儿最合适。
              全程不给任何提示：提示等于把入口告诉所有人，那藏它就没意义了。
              cursor-default + select-none：连点时既不像按钮，也不会把标题选成高亮。 */}
          {/* 两条规则都要压:仓库里所有「只响应指针、不进 tab 序」的元素都是这么写的
              （见 components/ComposerPopover.jsx、features/chat/ChatView.jsx）。
              只写一条的话另一条照样会报。 */}
          {/* biome-ignore lint/a11y/useKeyWithClickEvents: 隐藏入口，刻意不给键盘路径 */}
          {/* biome-ignore lint/a11y/noStaticElementInteractions: 隐藏入口，刻意不进 tab 序、不暴露为按钮 */}
          <h1
            data-testid="model-store-title"
            className="shrink-0 cursor-default select-none text-[26px] font-normal tracking-tight"
            onClick={handleSecretUnlockClick}
          >
            {copy.title}
          </h1>
          {guide === 'ready' ? (
            <div className="flex min-w-0 flex-1 flex-wrap items-center justify-end gap-3 sm:ml-8 sm:flex-nowrap">
              <IosSearchField
                value={query}
                compact
                className="min-w-[220px] flex-1 sm:max-w-[520px]"
                placeholder={copy.search}
                clearLabel={copy.clearSearch}
                inputTestId="model-store-search"
                onChange={(event) => setQuery(event.target.value)}
              />
              <button
                type="button"
                data-testid="model-store-storage"
                className={BTN_NEUTRAL}
                onClick={() => setStorageOpen(true)}
              >
                <Database size={14} className="opacity-70" />
                {copy.storageButton}
              </button>
              <button
                type="button"
                disabled={loading}
                data-testid="model-store-refresh"
                className={BTN_NEUTRAL}
                onClick={loadAll}
              >
                <RefreshCw size={14} className={loading ? 'animate-spin opacity-70 motion-reduce:animate-none' : 'opacity-70'} />
                {copy.refresh}
              </button>
            </div>
          ) : null}
        </div>
      </header>

      <main className="flex-1">
        <div className="mx-auto max-w-[1400px] pb-8 pt-5">
          {/* 测试商店开着时必须一直说出来:它的卡片和脚本都是打包在应用内的假数据,
              用户要是忘了自己开过,会把测试卡片当成正式上架的模型。 */}
          {guide === 'ready' && testStoreEnabled ? (
            <div
              data-testid="model-store-test-banner"
              className="mb-4 mx-3 flex flex-wrap items-center justify-between gap-2 rounded-[14px] bg-amber-50 px-3 py-2 dark:bg-amber-500/10"
            >
              <span className="text-[12px] text-amber-800 dark:text-amber-300">{copy.testStoreBanner}</span>
              <MsButton tone="ghost" onClick={handleDisableTestStore} data-testid="model-store-test-disable">
                {copy.testStoreDisable}
              </MsButton>
            </div>
          ) : null}
          {guide === 'ready' ? (
            <div className="mb-5 flex flex-wrap items-center gap-2 px-3" data-testid="model-store-filters">
              {FILTERS.map((item) => (
                <button
                  key={item.key}
                  type="button"
                  data-testid={`model-store-filter-${item.key}`}
                  aria-pressed={filter === item.key}
                  className={`${PILL_BASE} ${filter === item.key ? PILL_ON : PILL_OFF}`}
                  onClick={() => setFilter(item.key)}
                >
                  {item.label}
                </button>
              ))}
              {testStoreEnabled ? (
                <div className="ml-auto flex items-center gap-2" data-testid="model-store-source">
                  {SOURCES.map((item) => (
                    <button
                      key={item.key}
                      type="button"
                      data-testid={`model-store-source-${item.key}`}
                      aria-pressed={source === item.key}
                      className={`${PILL_BASE} ${source === item.key ? PILL_ON : PILL_OFF}`}
                      onClick={() => setSource(item.key)}
                    >
                      {item.label}
                    </button>
                  ))}
                </div>
              ) : null}
            </div>
          ) : null}
          {renderBody()}
        </div>
      </main>

      <StorageSettingsDialog
        open={storageOpen}
        onClose={() => setStorageOpen(false)}
        copy={copy}
        theme={theme}
        onSaved={showToast}
      />

      <StoreCardDialog
        card={detailCard}
        copy={copy}
        theme={theme}
        // 合并之后已下载的模型走库详情，所以这里理论上只会拿到未下载的；
        // 仍然按合并结果实算一次，不靠"理论上"。
        downloaded={!!detailCard && mergedModels.some((m) => m.id === detailCard.id && m.record)}
        downloading={!!detailCard && !!runningTask(downloadTaskKey(detailCard.id))}
        onClose={() => setDetailCard(null)}
        onDownload={(card) => {
          setDetailCard(null);
          handleDownload(card);
        }}
      />

      <ModelDetailDialog
        cardId={detailCardId}
        copy={copy}
        theme={theme}
        // 接入前要拿它查重：同一个本地模型重复接入会在品悟的模型列表里多出
        // 一行指向同一个服务的死数据（见 ModelDetailDialog.useInPinvou）。
        savedModels={bs?.savedModels}
        onToast={showToast}
        onClose={() => setDetailCardId(null)}
        onRedeploy={(record) => {
          setDetailCardId(null);
          handleDeploy(record);
        }}
      />

      {/* 卸载确认:给两个明确的选项,而不是"确定/取消"。
          权重删不删是用户的判断,不是我们的 —— 真机上既出过"卸载完磁盘一点没少"
          (白占几十上百 GB),也有"只想清掉部署、别让我重下几十 GB"的情况。
          两条文案各自把后果说全,尤其是"彻底删除不可撤销"那一条。 */}
      <MsDialog
        open={!!confirmUninstall}
        onClose={() => setConfirmUninstall(null)}
        theme={theme}
        labelledBy="model-store-uninstall-title"
        testId="model-store-uninstall-dialog"
      >
        {confirmUninstall ? (
          <>
            <MsDialogTitle id="model-store-uninstall-title">{copy.uninstallTitle}</MsDialogTitle>
            {uninstallOptionsFor(confirmUninstall) === 'delete-only' ? (
              // 只下载过、没部署过：没有服务可卸，摆两个按钮只会让用户在一个空动作
              // 上犹豫。这里退成单选项，文案也换成不提"停止服务"的那一句。
              <p className={`text-[13px] leading-6 ${TEXT_SECONDARY}`}>
                {copy.uninstallIntroDownloaded(confirmUninstall.card_name)}
              </p>
            ) : (
              <>
                <p className={`text-[13px] leading-6 ${TEXT_SECONDARY}`}>
                  {copy.uninstallIntro(confirmUninstall.card_name)}
                </p>
                <ul className={`mt-3 list-disc space-y-1.5 pl-5 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
                  <li>{copy.uninstallOptionKeep(formatBytes(confirmUninstall.size_bytes))}</li>
                  <li>{copy.uninstallOptionPurge(formatBytes(confirmUninstall.size_bytes))}</li>
                </ul>
              </>
            )}
            <div className="mt-5 flex flex-wrap justify-end gap-2">
              <MsButton tone="ghost" onClick={() => setConfirmUninstall(null)}>{copy.detailClose}</MsButton>
              {uninstallOptionsFor(confirmUninstall) === 'delete-only' ? null : (
                <MsButton
                  tone="neutral"
                  onClick={() => handleUninstall(false)}
                  data-testid="model-store-uninstall-keep"
                >
                  {copy.uninstallKeep}
                </MsButton>
              )}
              <MsButton
                tone="danger"
                onClick={() => handleUninstall(true)}
                data-testid="model-store-uninstall-confirm"
              >
                {copy.uninstallPurge}
              </MsButton>
            </div>
          </>
        ) : null}
      </MsDialog>

      {/* 强制从列表移除。破坏性且不可逆,所以走和卸载一样的二次确认;
          区别是这次要说的不是「会删掉什么」,而是「不会删掉什么」——用户在这一步
          最容易误以为自己终于把那 168 GB 清掉了。
          确认之后不直接关掉:同一个框换成结果页,把 LMM 报回来的残留路径摊开。 */}
      <MsDialog
        open={!!forceRemove}
        // 请求飞在半空中时不给关:这一刻关掉,用户既不知道成功没有,也再看不到
        // 残留路径了。
        onClose={forceRemoveBusy ? NOOP : closeForceRemove}
        theme={theme}
        labelledBy="model-store-force-remove-title"
        testId="model-store-force-remove-dialog"
      >
        {forceRemove && forceRemove.stage === 'confirm' ? (
          <>
            <MsDialogTitle id="model-store-force-remove-title">{copy.forceRemoveTitle}</MsDialogTitle>
            <p className={`text-[13px] leading-6 ${TEXT_SECONDARY}`}>
              {copy.forceRemoveIntro(forceRemove.cardName, formatBytes(forceRemove.sizeBytes))}
            </p>
            <ul className={`mt-3 list-disc space-y-1.5 pl-5 text-[13px] leading-6 ${TEXT_SECONDARY}`}>
              <li>{copy.forceRemovePointRecordOnly}</li>
              <li>{copy.forceRemovePointResidual}</li>
              <li>{copy.forceRemovePointAfter}</li>
            </ul>
            <div className="mt-5 flex justify-end gap-2">
              <MsButton tone="ghost" disabled={forceRemoveBusy} onClick={closeForceRemove}>
                {copy.detailClose}
              </MsButton>
              <MsButton
                tone="danger"
                busy={forceRemoveBusy}
                onClick={confirmForceRemove}
                data-testid="model-store-force-remove-confirm"
              >
                {copy.forceRemoveConfirm}
              </MsButton>
            </div>
          </>
        ) : null}
        {forceRemove && forceRemove.stage === 'result' ? (
          <>
            <MsDialogTitle id="model-store-force-remove-title">{copy.forceRemoveResultTitle}</MsDialogTitle>
            {/* warning 是 LMM 那边写好的、可以直接上界面的一句人话,原样显示。
                自己再改写一遍只会让两边的说法慢慢对不上。 */}
            <p className={`text-[13px] leading-6 ${TEXT_SECONDARY}`}>{forceRemove.result?.warning || ''}</p>
            {forceRemove.result?.residual_paths?.length ? (
              <>
                <div className={`mt-4 text-[13px] font-medium ${TEXT_PRIMARY}`}>
                  {copy.forceRemoveResidualTitle(forceRemove.result.residual_paths.length)}
                </div>
                {/* 路径要能整条读出来,所以 break-all 不截断:用户拿它去手动 rm。 */}
                <ul
                  data-testid="model-store-force-remove-residual"
                  className={`mt-2 space-y-1 rounded-[14px] bg-[#F0F4F9] p-3 text-[12px] leading-5 dark:bg-[#282A2C] ${TEXT_PRIMARY}`}
                >
                  {forceRemove.result.residual_paths.map((path) => (
                    <li key={path} className="break-all font-mono">{path}</li>
                  ))}
                </ul>
              </>
            ) : null}
            <div className="mt-5 flex justify-end">
              <MsButton tone="neutral" onClick={closeForceRemove} data-testid="model-store-force-remove-done">
                {copy.detailClose}
              </MsButton>
            </div>
          </>
        ) : null}
      </MsDialog>

      {/* 部署方案选择器(多 profile 卡片) */}
      <MsDialog
        open={!!profilePick}
        onClose={() => setProfilePick(null)}
        theme={theme}
        labelledBy="model-store-profile-title"
        testId="model-store-profile-dialog"
      >
        {profilePick ? (
          <>
            <MsDialogTitle id="model-store-profile-title">{copy.profileTitle}</MsDialogTitle>
            <div className="space-y-2">
              {profilePick.record.profiles.map((profile) => (
                <label
                  key={profile.id}
                  className={`flex cursor-pointer items-start gap-3 rounded-[14px] border border-[#E1E5EA] p-3 dark:border-[#2E3134] ${HOVER_SURFACE}`}
                >
                  <input
                    type="radio"
                    name="model-store-profile"
                    className="mt-1 accent-[#0B57D0]"
                    checked={profilePick.selectedId === profile.id}
                    onChange={() => setProfilePick((prev) => ({ ...prev, selectedId: profile.id }))}
                  />
                  <span className="min-w-0">
                    <span className={`block text-[13px] ${TEXT_PRIMARY}`}>{profile.name}</span>
                    {profile.description ? (
                      <span className={`block text-[11px] ${TEXT_SECONDARY}`}>{profile.description}</span>
                    ) : null}
                  </span>
                </label>
              ))}
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <MsButton tone="ghost" onClick={() => setProfilePick(null)}>{copy.detailClose}</MsButton>
              <MsButton onClick={confirmProfileDeploy} data-testid="model-store-profile-confirm">
                {copy.profileStart}
              </MsButton>
            </div>
          </>
        ) : null}
      </MsDialog>

      {/* 测试商店口令框。
          onClose 给一个空函数 = 点遮罩和按 Escape 都不关闭,只能点「取消」:
          误触把已经输了一半的口令清掉体验很糟,而这个框本来就是刻意难打开的。 */}
      <MsDialog
        open={unlockOpen}
        onClose={NOOP}
        theme={theme}
        labelledBy="model-store-unlock-title"
        testId="model-store-unlock-dialog"
      >
        <MsDialogTitle id="model-store-unlock-title">{copy.testStoreUnlockTitle}</MsDialogTitle>
        <p className={`mb-3 text-[13px] leading-6 ${TEXT_SECONDARY}`}>{copy.testStoreUnlockHint}</p>
        <input
          type="password"
          // biome-ignore lint/a11y/noAutofocus: 这个框是连点七次专门叫出来的,焦点不给它就得让用户再点一下才能输
          autoFocus
          className={INPUT_CLASS}
          placeholder={copy.testStorePasswordPlaceholder}
          data-testid="model-store-unlock-input"
          value={unlockInput}
          onChange={(event) => setUnlockInput(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && unlockInput) submitUnlock();
          }}
        />
        <div className="mt-5 flex justify-end gap-2">
          <MsButton tone="ghost" onClick={closeUnlock}>{copy.testStoreUnlockCancel}</MsButton>
          <MsButton disabled={!unlockInput} onClick={submitUnlock} data-testid="model-store-unlock-confirm">
            {copy.testStoreUnlockConfirm}
          </MsButton>
        </div>
      </MsDialog>

      <ModelStoreTaskPanel
        tasks={tasks}
        copy={copy}
        onCancel={cancelTask}
        onDismiss={dismissTask}
        onToggle={toggleTaskExpand}
        onAnswer={answerTask}
        onForceRemove={requestForceRemove}
      />

      <MsToast toast={toast} />
    </div>
  );
};

export { ModelStoreView };
