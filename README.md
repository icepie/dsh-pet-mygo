# dsh-pet-mygo

[dsh-pet](https://github.com/PC2005-cloud/dsh-pet) 的无 Electron / 无 DeepSeek Harness
桌面端：用 [MyGo](https://github.com/egoist/mygo) 的透明置顶窗 + 系统 webview
（macOS WKWebView / Linux WebKitGTK / Windows WebView2），按平台播放带 alpha 的
HEVC `.mov` 或 VP9 `.webm`，对外保持 dsh-pet 自己的 `/dsh-pet-7340` HTTP 契约。

**闸门实验已通过**：MyGo 透明无边框窗能透明渲染 HEVC-alpha mov（macOS）与
VP9-alpha webm（Linux WebKitGTK，实测 X11 + picom），因此整个桌宠可以不依赖
116MB 的 Electron 运行。

## 当前状态

已对齐 dsh-pet 原版桌宠核心交互：

- 透明、无边框、置顶、无阴影窗口，播放真实宠物动画（蓝毛小女仆）
- **双缓冲 `<video>` 交叉淡入**：切换动画无闪黑（等新片段解出首帧再淡入，旧片段保持在屏，避免切换瞬间空白）
- **动画链**：按权重（idle 10 / turn 5 / move 5 / 动作 80）连续自动选下一个，
  每个动画按其真实时长播完再切（解析 mov 的 `mvhd`），turn 翻转朝向、move 漫游、动作从 80+ 池随机
- **请求动画排队**：`POST /anim` / 碎碎念这类请求在当前片段播完后才接上（不切半截），
  所以工作状态不会把随机动画挤成只剩固定几个；点击回应则立刻抢占
- **页面自愈**：渲染进程死掉时自动 reload（`OnRenderProcessGone` + 页面 30s 心跳看门狗）
- **拖拽 / 甩抛**：按住拖动窗口跟随，松手速度快则抛掷（反弹+摩擦），慢则原地放下
- **点击穿透 + 动态命中**：透明区域不拦截鼠标（不遮挡下层窗口），命中区按当前动画的透明像素
  实时计算（`Pet.SetHitBox`），跳跃/举手等超出默认区域的姿势也能拖、能点
- **点击回应**：点击播 clicks 池动画 + Q 弹挤压（squish）
- **右键菜单**：按分类点播动画、清空气泡、回到初始位置
- **碎碎念**：周期自动弹一句 + 碎碎念动画；句子从预生成缓存（`whispers.json`）随机取，
  无缓存时用内置句子池降级。宠物运行时**不调模型**，批量生成一次能用很久
- **屏幕漫游 + 朝向镜像**：朝随机方向行走不走出屏幕，朝左时视频水平翻转
- **多显示器**：漫游与抛掷按窗口所在的显示器计算边界（`Screen.DisplayMatching`）
- **单元测试**：moov/EBML 时长解析、动画链权重、契约端点、配置读写、pet pack 隔离（`go test ./...`）
- **多宠物 / 多窗口**：每只宠物独立窗口、独立状态与气泡，bridge 用 `CallerWindow` 区分
- **多宠物种类（pet pack）**：`pets[].pack` 选种类，每个种类独立动画池与素材目录，
  菜单、随机动作、点击/碎碎念都只在本种类里挑；缺素材时记日志并回退内置种类，不拖垮其他窗口
- `/dsh-pet-7340` 契约端点：`GET /state`、`GET /config`、`POST /anim`（回 `playbackId`）、
  `GET /playback/{id}`（播放回执，状态含 `queued`）、`POST /playback/{id}/report`、`POST /whisper`、
  `POST /broadcast`、`POST /position`（归一化坐标）、`POST /throw`（抛掷+反弹+摩擦）、
  `GET /asset?name=`（按 `?pet=` 交给对应种类的素材文件，支持 Range），均按 `?pet=` 路由
- 状态结构与 dsh-pet 一致（`pets.<id>.{say,anim,facing} = {counter, data}`），
  因此之前写的 [pi 桥接扩展](../../.pi/agent/extensions/dsh-pet.ts) **零改动**直接可用
- 内置种类 106 个动画素材，两套格式：HEVC-alpha mov（macOS，134MB）与 VP9-alpha webm
  （Linux/Windows，56MB），页面按平台选目录与扩展名；素材由宿主经 `/asset` 提供，
  自定义种类可以放在 `resources/` 之外

## 平台支持

| 平台 | webview | 素材 | 透明 |
|---|---|---|---|
| macOS | WKWebView | HEVC-alpha `.mov`（`resources/anim`） | 实测可用 |
| Linux | WebKitGTK 4.1 / 4.0 | VP9-alpha `.webm`（`resources/anim-webm`） | 实测 X11 + picom |
| Windows | WebView2 | VP9-alpha `.webm` | 未实测 |

Linux 运行依赖：

- GTK 3 + WebKitGTK 4.1（没有时用 4.0）+ libsoup 3
- GStreamer 的 VP9 解码（`gst-plugins-good`）与 VP9 alpha（`gst-plugins-bad`，
  `vp9alphadecodebin`）
- 合成器（picom / KWin / Mutter / Hyprland 等）：裸 X11 没有合成器时透明窗会显示黑底
- 平铺窗口管理器（i3/sway）：Linux 窗口用 GTK utility 类型，保持浮动且全工作区可见，
  不会被平铺拉伸；启动时还会通过 WM IPC（`I3SOCK` / `SWAYSOCK`）去掉 i3/sway 给浮动窗
  画的焦点边框。若从没有该变量的环境启动，可在 WM 配置里加一行
  `for_window [title="^dsh-pet$"] border none`

点击穿透用 MyGo 的 `SetInputRegion`：Linux 上落到 X11 SHAPE / Wayland input region，
命中区按当前动画的透明像素动态更新；右键菜单打开时临时放大到整窗。macOS / Windows
由 MyGo 轮询指针切换。页面按 `mygo.platform` 选择 `anim/`+`.mov` 或 `anim-webm/`+`.webm`。

## 硬件加速

- **视频解码**：GStreamer 按 rank 自动选解码器。装了 VA-API 且驱动支持时选
  `vavp9alphadecodebin`（rank 267，硬解 VP9-alpha，输出 AV12），否则回退软件
  `vp9alphadecodebin`（rank 266）。本机（AMD RX 7700 XT）实测 `decodebin` 选中的是
  VA-API 硬解。
- **页面合成**：MyGo 默认关闭 WebKitGTK 的 DMA-BUF renderer（NVIDIA、虚拟机、容器里会
  白屏），此时页面合成走 CPU。GPU 主机可以 `DSH_PET_DMABUF=on` 交给 WebKit 自己决定；
  `auto` 只在 Wayland + 非 NVIDIA/虚拟机/容器时打开。本机 X11 + AMD 实测：
  关闭 10.8% CPU → 打开 8.1%（20 秒总进程采样），透明和 alpha 均正常。
- **窗口混合**：由桌面合成器负责。picom 默认 `xrender` 后端是 CPU 合成，改成
  `backend = "glx"` 才走 GPU；KWin/Mutter 默认已是 GPU。合成器会把透明窗当成整个矩形
  投影，看起来像一圈边框：可以在 picom 的 `rules` 里只排除宠物
  `{ match = "name = 'dsh-pet' || class_g = 'Dsh-pet-mygo-linux'"; shadow = false; }`，
  也可以让整个桌面无阴影（`shadow = false`）。
- macOS 走 VideoToolbox 硬解 + CoreAnimation 合成，无需开关。

## 与原版对比

实测数据（macOS arm64，`du` / `ps` / `wc`）：

| 指标 | dsh-pet-mygo | dsh-pet 原版 | 差距 |
|---|---|---|---|
| 分发包体积 | **16MB** 单二进制 | Electron 295MB（解压后）+ npm 包 98MB ≈ **393MB** | 小 96% |
| 运行时内存 | 66MB（实测 RSS） | Electron 典型 150~300MB | 省一半以上 |
| 进程数 | **1** | 宿主 + Electron helper + 每宠物 renderer | 单进程 |
| 代码量 | **1234 行**（Go 857 + 页面 377） | **25656 行** TS（host 13605 / client 5696 / shared 4891 / standalone 1464） | 少 95% |
| 素材体积 | mov 134MB（106 个） | webm 52MB（macOS 另需 mov 82MB） | 本项目更大 |
| 外部依赖 | 无（系统 webview） | Electron 首次下载 116MB | 零下载 |

```
分发包体积
  mygo 16M   ██
  原版 393M  ██████████████████████████████████████████████

代码量
  mygo 1234   ██
  原版 25656  ██████████████████████████████████████████████

进程数
  mygo 1     ██
  原版 多     ██████████
```

**本项目的优势**：零依赖零下载（不用等 Electron 116MB）、单二进制分发、代码量 1/20
（删掉了 DSH 宿主的 15k 行路由/事件/helper 进程管理/多屏坐标）、内存省一半以上。

**原版仍占优的地方**（如实列出）：余额动画 / 对话聊天 / 系统通知（缺模型与余额源）、
pet pack 的配置深度（原版每个种类可写独立权重、sleep 三段、人设；本项目只有
素材目录 + 按名字分池）、素材更小（webm 52MB vs mov 134MB）、多宿主生态与正式发布积累。

一句话：用 1/20 代码、1/25 体积实现约 80% 的核心桌宠体验，剩下的 20% 是依赖
外部凭证的能力与细节打磨。

## 运行

```sh
go run .        # 监听 http://127.0.0.1:3080，窗口播放 idle 动画
go test ./...   # 时长解析 / 动画链 / 契约 / 配置
```

两套素材都已入库，直接可跑。只有重新生成 macOS 的 HEVC-alpha mov 时才需要
macOS + ffmpeg + Xcode CLT 转码（webm 原素材来自 dsh-pet npm 包）：

```sh
./scripts/encode-movs.sh 待机呼吸休闲 工作状态-思考冒泡 工作状态-忙碌点按 \
  工作状态-清点归档 工作状态-原地踱步张望 工作状态-雀跃庆祝 工作状态-垂头叹气冒汗
```

驱动它（与 dsh-pet / pi 桥接扩展同款调用）：

```sh
curl -s -X POST 'http://127.0.0.1:3080/dsh-pet-7340/anim?pet=main' \
  -H 'content-type: application/json' -d '{"name":"工作状态-雀跃庆祝"}'

curl -s -X POST 'http://127.0.0.1:3080/dsh-pet-7340/broadcast?pet=main' \
  -H 'content-type: application/json' -d '{"text":"宿主骨架跑通啦","duration":6000}'
```

## 配置

配置文件：`~/.config/dsh-pet-mygo/config.json`（`DSH_PET_MYGO_CONFIG` 可覆盖路径）。

首次运行自动生成默认配置；拖动 / 漫游 / 抛掷后位置会自动写回（后台每 2 秒检查一次变化），
重启恢复原位。

```json
{
  "size": 0.7,
  "pets": [
    { "id": "main", "x": 700, "y": 220 },
    { "id": "cat1", "pack": "cat", "x": 300, "y": 120 }
  ]
}
```

- `size`：整体缩放比例（0.3~2.0，1 = 原生 640×360 画布，默认 0.7），窗口与命中区同步缩放。
- `pets`：每个元素一只宠物、一个独立窗口（`id` 不同即可多开）。
- `pack`：宠物种类，省略 = 内置蓝毛女仆；写名字则用 `pets/<名字>/` 下的素材（见下节）。
- `x` / `y`：窗口左上角的逻辑屏幕坐标。

> 注：macOS 上 MyGo 创建窗口时的 `X` 在小于约 221 时会被 AppKit clamp，所以本项目在
> 创建后用 `SetPosition` 显式定位，配置里的任意坐标都能生效。

### 换宠物（pet pack）

一个种类 = 配置里 `pack` 的一个值 + 一个素材目录，放在配置文件旁边：

```
~/.config/dsh-pet-mygo/
├─ config.json
└── pets/
    └── cat/
        ├── anim/*.mov        ← macOS（WKWebView）
        └── anim-webm/*.webm  ← Linux / Windows（WebKitGTK、WebView2）
```

```json
{ "id": "cat1", "pack": "cat", "x": 300, "y": 120 }
```

- 同 `pack` 的多只宠物共用一份素材与动画池，各自独立窗口、状态、气泡。
- **文件名决定用途**（与 dsh-pet 一致），种类只补自己需要的：
  - `待机呼吸休闲` → 待机循环；没有它时取第一个含「待机」的片段，再没有就取排序第一个。
  - `东张西望` → 转向；`螃蟹走路` / `原地漂浮踏步` / `原地左转奔跑` → 漫游（没有则不漫游）。
  - `点击回应-*` → 点击回应；`碎碎念-*` → 碎碎念；`工作状态-*` → 供 `/config` 消费方读的档位池；
    `余额-*` → 预留，不进随机池。
  - 其余片段自动进随机动作池。
- 素材规格不变：640×360 画布、带 alpha；按平台放对应格式，页面只播当前平台那一套。
- 少于 4 个池子不会报错：缺 `点击回应-*` 用随机动作代替，缺 `碎碎念-*` 只弹气泡不换动画。
- `pack` 目录不存在或没有素材文件时，**启动日志会点名**（`pack "cat" unusable for pet cat1 (...)`），
  该宠物回退内置种类继续跑，其他窗口不受影响。
- 改 `pack` 需要重启（每个窗口启动时绑定自己的种类）。

### 碎碎念句子（批量预生成）

宠物运行时**不调模型**：句子先批量生成到 `~/.config/dsh-pet-mygo/whispers.json`，
运行时随机取（避免连续重复），文件缺失时回退内置句子池。这样一批 ≈1k token 能用好几天。

```sh
# 用环境变量指向任意 OpenAI 兼容端点（不设则用 pi 的本地 magpie 代理）
DSH_PET_LLM_BASE=https://api.example.com/v1 \
DSH_PET_LLM_KEY=sk-xxx \
DSH_PET_LLM_MODEL=gpt-4o-mini \
./dsh-pet-mygo -generate-whispers -whisper-count 30

# pi 在跑时可以直接用默认（127.0.0.1:3425 magpie）
./dsh-pet-mygo -generate-whispers
```

## 架构

```
main.go                    透明窗 + /dsh-pet-7340 HTTP 服务 + 内存状态 + pet pack 扫描
resources/index.html       双缓冲 <video>，轮询 /state 切换动画 + 气泡 + 右键菜单
resources/anim/*.mov       HEVC-alpha 透明素材（内置种类，macOS）
resources/anim-webm/*.webm VP9-alpha 透明素材（内置种类，Linux/Windows）
scripts/encode-movs.sh     webm(VP9-alpha) → mov(HEVC-alpha) 转码
```

- 页面通过 `file://` 加载，`fetch` 到 HTTP 服务属跨域，服务端已加 CORS 头。
- 动画名即素材目录下的文件名（不含扩展名），视频统一走 `GET /asset?pet=&name=`。

## 素材：webm → HEVC-alpha mov

WKWebView 不认 VP9-alpha（黑底），需要 HEVC-with-alpha。ffmpeg 两条路都走不通
（`libx265` 打不开 `yuva*`，`hevc_videotoolbox` 静默丢 alpha），正确做法是复用
dsh-pet 上游的 AVFoundation 编码器：

```
ffmpeg 解码 webm(VP9-alpha) → BGRA 原始帧 → swift 编码器(AVAssetWriter + hevcWithAlpha) → mov
```

`hevc_alpha_encoder.swift`（dsh-pet 上游，MIT）已放在 `scripts/`，`encode-movs.sh`
会按需用 `xcrun swiftc` 编译它。

## 下一步

- 可选增强：碎碎念接模型（建议批量预生成句式缓存，避免每句一次调用烧 token）、
  对话聊天、余额动画、系统通知
- 素材入库策略（当前 106 个 mov 约 134MB，可改用 Git LFS 或脚本生成）

## License

MIT（复用 dsh-pet 素材与 `hevc_alpha_encoder.swift` 时保留其 MIT 声明）。
