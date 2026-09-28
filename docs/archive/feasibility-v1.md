# tele 可行性研究 v1（已归档）

> 本文件为 v1 版本，保留作历史参考。其中 NFS、WireGuard、swgp-go、fake-TCP（Phantun / tcpraw）、Outline 等方案已在 v2 中放弃，现行基线见 [../feasibility.md](../feasibility.md)。

> 状态：可行性研究（未开始实现）  
> 日期：2026-09-28  
> 研究对象：Claude Code 2.1.283（本机二进制逆向核实）、swgp-go v1.10.0、Musixal/tcpraw（替代 Phantun）、Linux 内核 WireGuard / NFS

## 0. 结论摘要

**结论：可行。** 推荐方案是：**Claude Code 仍在本地运行**，通过「路径同一性」（remote 目录挂到本地的**同一绝对路径**）加上 Claude Code 官方提供的执行注入点，把所有**执行类操作**（Bash、hooks、stdio MCP、ripgrep）转发到远端，把所有**文件类操作**（Read/Write/Edit/Glob 的底层 fs 调用）落到 NFS 上。Claude Code 本身不需要打补丁，也不需要改二进制。

| 需求 | 可行性 | 实现方式 | 主要风险 |
|---|---|---|---|
| `tele claude` 启动 | ✅ 高 | 启动器：建挂载命名空间 → 设环境变量 → exec `claude` | 无 |
| Bash 工具远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL` 指向 tele 提供的 shim | 依赖未公开的内部行为（cwd 文件、快照） |
| Hooks 远程执行 | ✅ 高（shell 形式）/ ⚠️ 中（exec 形式） | `CLAUDE_CODE_SHELL_PREFIX` | exec 形式 hook 可能绕过 prefix |
| stdio MCP 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL_PREFIX`（已核实会包裹 stdio MCP） | 无 |
| HTTP/SSE MCP | ⚠️ 中 | 改写 localhost URL 或经隧道端口转发 | 需要改写配置 |
| Read/Write/Edit/Glob/Grep | ✅ 高 | 同路径挂载（推荐 telefs，见 5A；或 NFS）；`USE_BUILTIN_RIPGREP=0` + 远程 `rg` shim | 缓存一致性（telefs 用 exec 屏障 + 失效推送解决） |
| 本地 MCP `tele-agent`（列出/切换主机、安装说明） | ✅ 高 | Go 写的 stdio MCP server，由启动器注入 | 热切换的边界情况 |
| WireGuard | ✅ 高 | 内核 WG + `wgctrl`（没有内核模块时退回 wireguard-go） | 需要 root |
| swgp-go（可关） | ✅ 高 | 本项目已改为 AGPL-3.0，可**直接作为库嵌入**（仅在保留 WG 后端时需要） | `-2026` 模式需要时钟同步 |
| fake-TCP 层（可关，自愈；替代 Phantun） | ✅ 高 | 只用 Musixal/tcpraw（MIT，纯 Go），作为库嵌入 teled/tele-server；只需 CAP_NET_RAW，无需 TUN/NAT；本机实测可用 | 单人维护（vendor 并固定 commit）；需修补 iptables 规则残留问题 |
| 文件层 | ✅ 高 | **推荐 telefs**（FUSE + tele 自有通道，本地和远端都不需要 root，已实测 userns 挂载）；备选 NFSv4.2 | telefs 需要自研（3–4 周）；NFS 需要 root，且缓存一致性弱 |
| Go 语言、仅 Linux | ✅ 高 | 整个生态都有成熟 Go 库 | — |

最大的不确定性**不在网络层**，而在 **Claude Code 的内部行为**：临时文件路径、shell 快照、cwd 追踪等都没有公开文档，各版本之间可能变化。因此第一件事是做一个 **P0 原型**，把第 3 节列出的行为逐项跑通，并建立一个**跨 Claude Code 版本的兼容性测试**。

---

## 1. 目标与非目标

**目标**

1. `tele claude [args...]` 与 `claude [args...]` 行为一致，只是「世界」在远端：Bash、hooks、MCP server 都在远端执行，文件都是远端的文件。
2. 远端主机**不需要**安装 Node 或 Claude Code，也**不存放** Anthropic 凭证。凭证、会话历史、`~/.claude` 都留在本地。
3. 本地自动多一个 MCP server `tele`：Claude 可以列出主机、切换主机（包括切回 `local`），并能拿到远端服务端的安装指引。
4. 传输层：WireGuard → [swgp-go，可关] → [fake-TCP（Musixal/tcpraw，替代 Phantun），可关，要求自愈]。
5. 文件层用 NFS。全部用 Go 实现，只支持 Linux。

**非目标**：macOS/Windows、多用户共享同一个远端会话、替代 SSH 做通用远程管理、Claude Code 自带的 bubblewrap 沙箱与远程执行的组合（见第 8 节）。

---

## 2. 总体架构

```
┌──────────────────────── 本地 (Linux) ─────────────────────────┐        ┌──────────────── 远端 (Linux) ────────────────┐
│                                                               │        │                                               │
│  tele claude ──► [userns+mountns]                             │        │  tele-server (root, systemd)                  │
│                   │  /home/u/proj  ◄── bind ── NFS mount ◄────┼── NFS ─┼── nfsd  export /home/u/proj → 10.77.0.1 only  │
│                   │  $CLAUDE_CODE_TMPDIR (共享 scratch)        │        │         export /var/lib/tele/s/<sid>          │
│                   ▼                                           │        │                                               │
│                 claude (Node, 原版未改动)                        │        │  exec 服务 (只监听 WG IP)                      │
│                   │ Bash  → CLAUDE_CODE_SHELL = tele-sh ──────┼─ RPC ──┼──► fork/exec 在远端, 转发 stdio/信号/退出码     │
│                   │ Hook  → SHELL_PREFIX = tele-exec ─────────┤        │                                               │
│                   │ MCP   → SHELL_PREFIX = tele-exec ─────────┤        │                                               │
│                   │ Grep  → rg (PATH shim) ───────────────────┤        │                                               │
│                   │ Read/Write/Edit → fs 调用 → NFS            │        │                                               │
│                   └ MCP "tele" (本地 stdio)                     │        │                                               │
│                                                               │        │                                               │
│  teled (root 守护进程): WG / swgp / faketcp / NFS 挂载 / 自愈 │        │  wg ◄─ swgp server ◄─ fakeTCP(tcpraw)          │
│   wg0 ─► swgp client ─► fakeTCP(tcpraw) ══ fake-TCP/UDP ══════╪════════╪══►                                            │
└───────────────────────────────────────────────────────────────┘        └───────────────────────────────────────────────┘
```

**二进制划分（单一 Go 模块，多个入口）**

| 二进制 | 运行位置 | 权限 | 作用 |
|---|---|---|---|
| `tele` | 本地 | 普通用户 | CLI：`tele claude`、`tele host add/ls/rm`、`tele status`；`tele mcp` 子命令即名为 `tele-agent` 的 MCP server |
| `teled` | 本地 | root（systemd） | 管理 WG 接口、swgp 子进程、进程内 fake-TCP relay、NFS 挂载、健康检查与自愈；通过 unix socket 接受 `tele` 的请求（用 SO_PEERCRED 鉴权） |
| `tele-sh` / `tele-exec` / `rg` shim | 本地 | 普通用户 | 注入给 Claude Code 的执行垫片，把请求交给 `teled` 复用的多路连接 |
| `tele-server` | 远端 | root（systemd） | WG 服务端、swgp 服务端托管、进程内 fake-TCP relay、NFS 导出管理、exec 服务、安装与配对 |

---

## 3. Claude Code 注入点（已在 2.1.283 二进制中核实）

我对本机 `/opt/claude-code/bin/claude`（bun 编译的单文件，内嵌压缩 JS）做了字符串层面的逆向，确认了以下事实：

### 3.1 `CLAUDE_CODE_SHELL`：替换 Bash 工具使用的 shell

```js
async function QAn(){let e=a.CLAUDE_CODE_SHELL;
  if(e) if((e.includes("bash")||e.includes("zsh")) && await lbe(e)) return e  // 路径里必须含 bash/zsh 且可执行
  ...
```

- 路径中**必须**包含 `bash` 或 `zsh`，所以 shim 需要命名成类似 `/usr/lib/tele/bin/bash` 的形式。
- 启动参数为 `[shell, "-c", "-l", commandString]`。
- `commandString` 的结构是：`source <快照> && <extglob 关闭> && eval '<用户命令>' && pwd -P >| <cwd 文件>`。

**影响**：shim 会收到整段脚本，把它原样交给远端 `bash -c -l` 执行即可。但脚本里引用了两个**本地路径**：shell 快照和 cwd 文件。所以这两个路径在远端必须以**同一路径**存在，而且内容必须双向可见。见 3.4。

### 3.2 `CLAUDE_CODE_SHELL_PREFIX`：同时包裹 Bash、hooks 和 stdio MCP

核实到三处用法：

```js
// Bash 工具：
Xe = Ne.join(" && "), tt = a.CLAUDE_CODE_SHELL_PREFIX; if(tt) Xe = g9(tt, Xe)
// hooks（shell 形式，spawn 时 shell:true → /bin/sh -c）：
Qn = !Xe && !tt && Wt ? g9(Wt, bt) : bt
// stdio MCP：
let w = a.CLAUDE_CODE_SHELL_PREFIX || r.command,
    F = a.CLAUDE_CODE_SHELL_PREFIX ? [qr([r.command, ...r.args])] : r.args
// 包裹方式：
function g9(e,n){ ... return `${qr([e])} ${qr([n])}` }   // → 'prefix' '<整条命令字符串>'
```

**影响**：

- 只设一个 `CLAUDE_CODE_SHELL_PREFIX=/usr/lib/tele/bin/tele-exec`，就能把 **hooks 和 stdio MCP** 全部转到远端。`tele-exec` 拿到的是一条 shell 字符串，远端用 `sh -c` 执行。MCP 的 stdin/stdout 是长连接的 JSON-RPC 流，`tele-exec` 做双向流转发即可。
- 缺口：hooks 的 **exec 形式**（`command` + `args`，代码里的 `Vt` 分支）直接 spawn，看起来**不经过** prefix。对策是启动器通过 `--settings` / `--setting-sources` 注入一份改写过的设置，或者在 P0 中确认后提 issue。
- Bash 工具如果同时设了 SHELL 和 PREFIX，会被**包两层**。可以只用 SHELL（shim 负责 Bash），PREFIX 负责 hooks 和 MCP；shim 识别出 `tele-exec` 前缀后自动去掉一层。具体做法在 P0 里定。

### 3.3 `USE_BUILTIN_RIPGREP`：让 Grep/Glob 使用 PATH 里的 `rg`

```js
if (Wo(a.USE_BUILTIN_RIPGREP)) { let {cmd:n} = rm("rg",[]); if(n!=="rg") return {mode:"system",command:n,...} }
```

设 `USE_BUILTIN_RIPGREP=0`（falsy），并把 tele 的 `rg` shim 放到 PATH 前面，Grep/Glob 就会在**远端**用远端本地磁盘执行 ripgrep，不再逐个文件走 NFS。这是性能上最关键的一点：大仓库的全量扫描只需 1 个 RTT，而不是成千上万次 NFS LOOKUP/READ。

### 3.4 临时目录与 `~/.claude` 下的共享路径

- `CLAUDE_CODE_TMPDIR`：可以重定向 Claude 的每 uid 临时目录（cwd 文件、后台任务输出等都在这里）。
- `~/.claude/shell-snapshots/`：shell 快照；`~/.claude/session-env/`：`CLAUDE_ENV_FILE`，SessionStart hook 用它持久化环境变量。

**方案**：`tele-server` 为每个会话在远端创建 `/var/lib/tele/s/<sid>/{tmp,shell-snapshots,session-env}` 并通过 NFS 导出；本地在会话的挂载命名空间里把它们**绑定到同一路径**，并把 `~/.claude/shell-snapshots`、`~/.claude/session-env` bind 到对应子目录。远端执行时，exec 服务也在一个挂载命名空间里做镜像绑定，让 `$HOME/.claude/shell-snapshots` 这个本地路径在远端也能解析到。最简单的做法是让 exec 服务把**本地的 home 路径**映射成同一个 scratch 目录。

快照由 shim 在**远端**生成（因为 `CLAUDE_CODE_SHELL` 也用于生成快照），里面记录的是**远端**的 PATH、别名和函数。这正是我们想要的。

### 3.5 其它内置行为

| 行为 | 处理 |
|---|---|
| Read/Write/Edit/NotebookEdit | 进程内 fs 调用 → 同路径 NFS，天然透明 |
| 内部 `git` 调用（生成 git status 上下文等） | 走 NFS 在本地执行即可；可选：PATH 里放 `git` shim 转发远端，减少 NFS 往返 |
| WebFetch / WebSearch | 在本地或 Anthropic 侧执行，与主机无关（出网 IP 是本地的，需要写进文档） |
| 超时与中断：先 SIGTERM，再 SIGKILL（tree-kill） | shim 转发可捕获的信号；**SIGKILL 无法捕获** → 远端以「连接断开 = 杀进程组」兜底（租约 + 心跳） |
| 后台 Bash（run_in_background） | 同上，输出文件位于共享的 `CLAUDE_CODE_TMPDIR` |
| 系统提示中的 OS 版本、平台 | 显示的是本地信息；由 `tele-agent` MCP 的 instructions 注入「当前主机」的真实信息 |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 路径同一性 → 与在远端直接运行时的项目键一致；切换主机后 `--resume` 仍然可用 |

---

## 4. 路径同一性与命名空间

**原则**：远端目录 `/X` 在本地会话里看到的也是 `/X`。这样 Claude 发出的所有绝对路径（Read 的参数、Bash 的 cwd、hook 的 `CLAUDE_PROJECT_DIR`）不需要任何改写，在两边都成立。

**实现**：

1. `teled`（root）在初始命名空间里把远端导出挂载到 `/run/tele/mnt/<host>/<export-id>`。NFS **不能**在非特权用户命名空间里挂载（没有 `FS_USERNS_MOUNT`），所以这一步必须由特权方完成。
2. `tele claude`（普通用户）执行 `unshare(CLONE_NEWUSER|CLONE_NEWNS)`，把 uid/gid 映射为**自身**（`--map-current-user` 语义），因此 Claude 看到的仍是自己的 uid。在新 userns 中它拥有 CAP_SYS_ADMIN，可以把 `/run/tele/mnt/...` **bind** 到目标路径。bind 已有挂载在 userns 中是允许的。
3. 目标路径在本地不存在、而且父目录不可写时（比如 `/srv/app`），在父目录挂一层 tmpfs 并重建目录骨架，再 bind。这种做法要在文档里写明，因为它会遮住本地同名目录。
4. 完成后 exec `claude`，同时设好环境变量、`--mcp-config`（注入 `tele-agent` MCP）等。

**兼容性注意**：

- Ubuntu 23.10+ 默认开启 `kernel.apparmor_restrict_unprivileged_userns=1` → 需要随包安装 AppArmor profile（`userns,` 权限），或者退化为由 `teled` 代为 fork 进程并通过 pty 代理终端（参考 `machinectl shell`/`run0` 的做法）。
- 一些发行版关闭了 `user.max_user_namespaces` → 同样退化到 `teled` 代理模式。

**uid 映射**：NFS 的 AUTH_SYS 用数值 uid 做鉴权。本地 uid 和远端 uid 往往不同。方案是远端导出时对**该 WG 对端 IP** 使用 `all_squash,anonuid=<远端用户 uid>,anongid=<gid>`，安全边界由 WG 保证。代价只是本地 `ls -l` 显示远端的数字 uid，属于外观问题。

---

## 5. 文件层：NFS（备选；推荐方案见 5A）

**选型**：远端用内核 nfsd，NFSv4.2（单端口 2049，便于在 WG 上跑；支持服务端 copy 和 sparse）；本地用内核 NFS 客户端。不推荐用户态的 go-nfs：它只支持 v3，性能和一致性也不如内核实现。

**导出**：`tele-server` 在 `/etc/exports.d/tele.exports` 中管理条目，并调用 `exportfs -ra`：

```
/home/alice/proj        10.77.0.1/32(rw,sync,no_subtree_check,all_squash,anonuid=1000,anongid=1000,sec=sys)
/var/lib/tele/s/<sid>   10.77.0.1/32(rw,sync,no_subtree_check,all_squash,anonuid=1000,anongid=1000)
```

**一致性（重点风险）**：

- 本地写 → 远端命令读：close-to-open 语义保证 close 时已刷回服务端。服务端本地进程直接读底层文件系统 → ✅ 一致。
- 远端命令写 → 本地 Read：`open()` 会重新 GETATTR（cto）→ 数据正确；但 **`stat()` 走属性缓存**（默认 acregmin 3s）。Claude 的「文件自上次读取后已修改」检测依赖 mtime，可能出现误判或漏判。
- 建议的挂载参数：`vers=4.2,proto=tcp,hard,actimeo=1,lookupcache=positive,nconnect=4`；提供可选的 `noac` 严格模式。另外，shim 每次远程命令结束后，teled 可以**主动失效**：对 cwd 执行一次 `open(O_RDONLY)` 或 `stat` 刷新，或者由远端 exec 服务返回「本次修改的文件列表」（用 fanotify 采集），对这些路径做针对性刷新。这是一个值得原型验证的优化。
- inotify 不会跨 NFS 传递远端的改动。Claude 对设置文件的热加载会失效，影响很小。

**性能预估**（RTT = 30ms 时）：一次 Read ≈ 2–3 个 RTT（LOOKUP/OPEN/READ compound）≈ 60–90ms；一次 Bash ≈ 1 RTT + 执行时间；Grep 走远端 rg ≈ 1 RTT + 扫描时间。整体体验与在远端 SSH 里直接用 Claude 相当，瓶颈仍然是模型响应。

---

## 5A. 不用 NFS 的方案（推荐：telefs = FUSE + tele 自有通道）

**结论：可以不用 NFS，而且去掉它以后整体更简单，也更容易免 root。** NFS 在本方案里带来的麻烦几乎都与它本身有关：

- 本地挂载必须 root，因为 NFS 不能在 user namespace 里挂载；
- 远端要配置 nfsd 和 exports，也要 root；
- 数字 uid 映射，需要 `all_squash`；
- 属性缓存导致读到旧数据，而且服务端无法主动通知客户端失效；
- 内核 NFS 客户端只能跑在内核 WG 上，所以 WG 也被迫要 root。

### 5A.0 既然命令都转发了，还需要挂载文件系统吗？

**需要，但只是为了 Claude Code 进程自己用。** 命令类操作（Bash、hooks、stdio MCP、`rg`）确实都转发到了远端，但 Claude Code 还有一大块功能是在**本地进程内**直接调用 fs，而不是启动命令：

| 进程内的文件访问 | 没有挂载时的后果 |
|---|---|
| Read / Write / Edit / NotebookEdit 工具（bun 的 fs 调用） | 直接读写的是本地磁盘，看不到远端文件 |
| 启动时加载项目里的 `CLAUDE.md`、`.claude/settings*.json`、`.mcp.json`、`.claude/{skills,commands,agents}` | 项目配置、hooks 定义、技能全部丢失 |
| Bash 的 spawn `cwd` 参数，以及执行后读取 cwd 文件、检查目录是否存在（「Shell cwd was reset to …」逻辑） | 本地不存在该目录 → spawn 报 ENOENT，或者 cwd 被重置回项目根 |
| 编辑前必须先读、文件是否被外部修改（基于 mtime） | 无法判断，Edit 被拒绝或误判 |
| checkpoint / `/rewind`（回滚文件） | 回滚的是本地文件，远端不受影响 |
| `@` 文件引用与补全、图片/PDF 读取、git 状态上下文 | 失效或错误 |

**不挂载也不是完全做不到**（即 5A.1 的方案 C）：

- 用 `--disallowedTools Read,Write,Edit,NotebookEdit,Glob,Grep` 禁掉内置文件工具，改由 `tele-agent` MCP 提供远程版本；
- 启动时从远端拉取 CLAUDE.md、settings 和 `.mcp.json`，通过 `--append-system-prompt`、`--settings`、`--mcp-config` 注入；
- 在本地 userns 里用 tmpfs 建一个**只有目录骨架**的同路径树，满足 spawn cwd 的要求；每次命令结束后，shim 按远端的 pwd 补建目录。

代价是失去「透明」：编辑安全检查、diff 展示、rewind、`@` 引用、图片读取、权限规则里的路径匹配都会失效；模型也要改用非原生的工具名。因此它只适合作为**降级模式**（例如 FUSE 不可用的环境），不作为默认方案。

**折中**：telefs 只承担 Claude 进程自身的文件访问，重负载（搜索、构建、测试、git）都在远端直接执行，所以它的访问量和性能要求都很低，主要是 Read/Edit 单个文件和启动时加载配置。

### 5A.1 候选对比

| 方案 | 透明度 | 权限需求 | 一致性 | 工作量 | 结论 |
|---|---|---|---|---|---|
| **A. telefs：FUSE（go-fuse）+ 走 tele 自己的 WG/yamux 通道** | 完全透明（真实文件系统，同路径） | 本地**无需 root**（在 userns 里挂载）；远端以**目标用户**身份运行 | **服务端主动推送失效**，可以做到比 NFS 更强 | 3–4 周 | **推荐** |
| B. 双向同步（类似 mutagen）：本地保留镜像，以每次远程 exec 作为同步屏障 | 基本透明；文件工具的延迟最低 | 无需 root | 需要处理后台进程持续写入、冲突、大目录（node_modules 等）的忽略规则 | 3–5 周 | 备选，适合高 RTT 链路 |
| C. 禁用内置文件工具（`--disallowedTools`），由 `tele-agent` MCP 提供 remote_read/edit/glob | **不透明**：失去 Claude 原生的读后编辑检查、diff 展示、checkpoint/rewind、@ 引用和图片读取；远端 CLAUDE.md 和 `.claude/` 需要另外注入 | 无需 root | 强一致 | 1–2 周 | 只作为降级手段 |
| D. LD_PRELOAD 或 ptrace/seccomp 劫持文件系统调用（proot 式） | 透明 | 无需 root | 强一致 | 高 | 否：Claude 是 bun 单文件二进制，劫持很脆弱，有性能代价 |
| E. 现成的 sshfs/SFTP | 透明 | 需要 fusermount | 与 NFS 类似，没有失效推送 | 低 | 否：要依赖 SSH 通道，不走你指定的传输栈 |

### 5A.2 telefs 设计

```
本地 claude 进程 ──VFS──► FUSE (/home/u/proj, 在会话 userns 内) ──► tele 启动器内的 telefs 客户端
     ──yamux stream (WG 隧道内)──► tele-server 内的 telefs 服务端 (以目标用户身份) ──► 远端本地文件系统
                                            ▲ inotify/fanotify 事件 → 失效推送
```

- **客户端**：[hanwen/go-fuse v2](https://github.com/hanwen/go-fuse)（v2.11.0，BSD 许可，gocryptfs 等项目在用）。它支持 `DirectMount`（自己调用 `mount(2)`，不需要 fusermount），以及 `NotifyContent` / `NotifyEntry` / `NotifyDelete` 等内核缓存失效接口。
- **协议**：FUSE 操作一一映射成 RPC：lookup、getattr、readdirplus、open、read、write、create、mkdir、unlink、rename、symlink、readlink、setattr、fsync、statfs，以及少量 xattr。用 protobuf 或 msgpack 编码，跑在已有的 yamux 连接上，与 exec 共用同一条通道。协议状态很薄：inode 用 (dev, ino, generation) 标识。另一个选择是直接采用 9P2000.L（例如 `hugelgupf/p9`，gVisor 系），再加一条失效侧信道。
- **一致性模型**（比 NFS 强，这是选择 telefs 的主要理由）：
  1. **exec 屏障**：远端每条命令结束时，tele-server 汇总该命令执行期间 inotify 记录到的变更路径，**在返回退出码之前**推送失效。客户端收到并执行 `Notify*` 之后，shim 才返回。这样可以保证「Bash 改了文件 → 紧接着 Read」这一最常见的顺序一定读到新内容。Claude 基于 mtime 的「文件已被修改」检测也因此变得准确。
  2. **后台变更**：后台进程、hooks、MCP server 造成的变更同样通过 inotify 流异步推送。
  3. 因为有了推送，内核的 attr/entry 缓存可以放心设置较长的 TTL（例如 30–60 秒），比 NFS 的 `actimeo=1` 快得多。只有推送断开时才退回短 TTL。
  4. 写入路径：客户端 write 直接透传；fsync/close 时确认服务端已落盘。先不开 writeback cache，保证远端命令立即可见。
- **inotify 限制**：大仓库需要调高远端的 `fs.inotify.max_user_watches`，由 install 负责检查和调整。有 root 时也可以改用 fanotify `FAN_MARK_FILESYSTEM`，不受数量限制。
- **性能**：未命中缓存的操作 ≈ 1 RTT（与 NFS 相同）；命中缓存则为本地速度。Grep/Glob 仍然走远端 `rg` shim。可以预取小文件（首次 readdirplus 时顺带返回 <4 KB 文件的内容），减少往返。

### 5A.3 本机实测（Linux 6.18，go-fuse v2.11.0）

我写了一个最小启动器验证了关键路径：普通用户（uid 1001）在 `CLONE_NEWUSER|CLONE_NEWNS` 中把 uid/gid **映射为自身**，并用 ambient `CAP_SYS_ADMIN` 执行 `DirectMountStrict` 挂载 FUSE。随后子进程能读取已有文件，也能新建文件；宿主侧看到的属主是正确的 `1001:1002`。实测踩到的三个坑都有解：

| 现象 | 原因 | 对策 |
|---|---|---|
| 挂载时 EPERM | 本测试容器的 `/dev/fuse` 权限是 0600（常规发行版是 0666） | install 时检查；用户环境通常不存在这个问题 |
| 读正常、create 返回 EACCES | 内核把 FUSE 根 inode 初始化为 uid 0，而 uid 0 在 userns 中未映射，VFS 的 `HAS_UNMAPPED_ID` 因此拒绝写入 | 挂载后立即 `stat` 挂载点，触发 GETATTR 刷新根 inode 属主 |
| `BACKING_OPEN` 报 EPERM | go-fuse 的 FUSE passthrough 需要**初始命名空间**的 CAP_SYS_ADMIN | telefs 本来就是远程文件，不使用 passthrough |
| 子进程 CapEff 仍带 CAP_SYS_ADMIN | ambient capability 会在 exec 时被继承 | 启动 claude 前执行 `PR_CAP_AMBIENT_CLEAR_ALL`，或经过一个小的 exec 中转来清除 |

另外，Ubuntu 23.10+ 的 AppArmor userns 限制（见第 4 节）仍然适用。

### 5A.4 去掉 NFS 带来的连锁简化

1. **本地可以完全不要 root**：内核 NFS 客户端不再需要内核 WG，WG 可以改用 **wireguard-go + gVisor netstack 在进程内运行**（不需要 TUN），telefs 和 exec 的 TCP 都跑在 netstack 上。fake-TCP（Musixal/tcpraw）只需要给二进制 `CAP_NET_RAW`。`teled` 可以从「root 守护进程」降级为普通用户服务。
2. **远端也基本不要 root**：tele-server 以目标用户身份运行，文件和 exec 天然使用正确的 uid，不需要 exports、nfsd 和 `all_squash`。只有启用 fake-TCP 时需要 `CAP_NET_RAW`（通过 setcap 或 systemd `AmbientCapabilities` 授予），并且要能写那条可选的 TTL 丢弃规则。
3. **路径同一性更容易**：FUSE 直接挂在会话 userns 的目标路径上，不需要「特权方先挂载、再 bind」这两步。
4. **共享 scratch**（`CLAUDE_CODE_TMPDIR`、shell-snapshots、session-env）同样用 telefs 挂载。

**代价**：telefs 需要自己写，约 3–4 周，其中测试占大头：可以用 pjdfstest 或 xfstests 的子集做 POSIX 语义回归，再用 git、npm、cargo 等真实工作负载做回归。FUSE 的上下文切换开销在 RTT 面前可以忽略。

## 6. 传输层

### 6.1 分层与端口链

```
客户端:  wg0(10.77.x.1) → udp 127.0.0.1:Ps ─► swgp-client → udp 127.0.0.1:Pf ─► fakeTCP relay(teled 内) ═fake-TCP═►
服务端:  ═►fakeTCP relay(tele-server 内) :443/tcp → udp 127.0.0.1:Qs ─► swgp-server → udp 127.0.0.1:51820 ─► wg0(10.77.x.2)
```

两层都可以关掉。关掉某一层时，WG peer 的 endpoint 直接指向下一层（或远端公网地址）。四种组合都要进入测试矩阵：{WG}、{WG+swgp}、{WG+faketcp}、{WG+swgp+faketcp}。

**拓扑**：本地一个 `tele0` 接口，**每台远端主机一个 peer**，每台主机独立一条 swgp 进程 + fake-TCP relay 链。地址按主机分配 `10.77.<n>.0/30`，或者 ULA `fd7e:1e::/64`。AllowedIPs 只包含对端的那个 /32，**不做**全局路由，避免影响本机的其他流量。

### 6.2 WireGuard

- 首选内核 WG（Linux ≥ 5.6），通过 `golang.zx2c4.com/wireguard/wgctrl` 配置，地址和路由用 `vishvananda/netlink`。
- 没有内核模块时退回 `wireguard-go`（可以作为库嵌入，MIT 许可）。
- `PersistentKeepalive=25`，用来维持 NAT 映射和 fake-TCP 流的状态。

### 6.3 swgp-go（可关）

- v1.10.0，纯 Go，`service.Config` / `Manager` 是**导出的 API**，技术上可以直接嵌入。
- ~~许可证冲突~~：本项目已改为 **AGPL-3.0**，swgp-go 的 `service` 包可以直接编译进 tele。（下面是旧版分析，保留作参考）应当作为**独立子进程**分发和托管（聚合分发，不构成衍生作品），由 teled / tele-server 生成 JSON 配置并监管进程。另一种选择是在安装时从上游 release 下载，并固定 sha256 校验。
- ⚠️ 上游要求 **go ≥ 1.26**（本环境是 1.24.7）。这只影响我们自己构建它的情况。
- 模式：默认用 `zero-overhead-2026`，数据包零开销、**不影响 MTU**；可选 `paranoid-2026`（全包 AEAD 并填充到 MTU，会略微降低 MTU、增加带宽）。**`-2026` 模式带重放保护，要求两端时钟同步**，所以 tele-server 安装时要检查 NTP，teled 健康检查要报告时钟偏差；时钟不可控时回退到旧版 `zero-overhead`。

### 6.4 fake-TCP 层（可关，需要自愈）：只用 Musixal/tcpraw

> 决策：fake-TCP 层**只**采用 [Musixal/tcpraw](https://github.com/Musixal/tcpraw)。它是 [xtaci/tcpraw](https://github.com/xtaci/tcpraw)（kcptun `--tcp` 模式使用的库）的性能优化 fork，MIT 许可，纯 Go，没有 cgo。最初需求里的 Phantun 因此由它**替代**。两者要解决的问题相同（把 UDP 伪装成 TCP，且不做重传），但线协议不兼容。由于两端都由 tele 控制，这一点没有影响。

#### 6.4.1 为什么是它

**已在本环境实测**（root，loopback，`BenchmarkEcho`，1 KB 往返）：

| 实现 | ns/op | 吞吐 | 内存/op | 分配次数/op |
|---|---|---|---|---|
| xtaci/tcpraw | 78,325 | 13.07 MB/s | 6,070 B | 54 |
| **Musixal/tcpraw** | **43,486** | **23.55 MB/s** | **2,424 B** | **10** |

这项基准是往返延迟型测试，不代表吞吐上限，但能说明热路径开销约减半、分配少了 80%。Musixal 的主要改动包括：缓冲池、手写 TCP 包解析（不走 gopacket 的解码分配）、seq/ack 改用原子变量、每个 socket 挂 BPF 过滤器减少无关抓包，以及去掉全局 10ms 时钟 goroutine。

**工作机制**（通过源码核实）：

1. 先用**真实内核 TCP** 完成三次握手。中间设备看到的是标准的 Linux TCP 握手，fingerprint 模拟 Linux：window 65535、TTL 64、NOP,NOP,Timestamp 选项。
2. 握手后把内核 socket 的 **TTL 设为 1**，让内核自己发出的 ACK 和重传在第一跳就被丢弃；同时用 `io.Copy(io.Discard, tcpconn)` 持续排空内核接收缓冲区，避免堆积。
3. 数据用 raw IP socket（`ip:tcp`）加 BPF 过滤收发，按真实的 seq/ack 推进，因此对 NAT 友好。
4. 对外暴露 `net.PacketConn`（`Dial` / `Listen`），可以直接嵌入 teled 和 tele-server，**不需要额外进程，不需要 TUN，不需要 NAT/DNAT，也不需要 `ip_forward`**。

**权限**：需要 `CAP_NET_RAW`（raw socket）。另外它会用 `go-iptables` 追加一条 `OUTPUT -m ttl --ttl-eq 1 ... -j DROP` 规则，用来吞掉 TTL=1 包触发的 ICMP Time Exceeded。这条规则需要 `CAP_NET_ADMIN`，而且是**可选**的：规则写入失败也能工作，只是会多一些 ICMP 噪声。

**与其他候选的比较（已排除）**：

- Musixal/ZeroTCP：基于 AF_PACKET，只支持 IPv4，用 `sudo iptables` 调命令，错误路径里有 `log.Fatal`，还是 pre-release，**没有 LICENSE 文件**，不适合采用。
- Musixal/Backhaul：没有 fake-TCP 传输，且是 AGPL 许可。
- Phantun（上游或 Go 移植）：需要 TUN 和 NAT，而且没有现成的 Go 实现。按本决策不再考虑。

#### 6.4.2 集成方式

```
客户端: wg0 → udp 127.0.0.1:Ps → [swgp-client 子进程, 可关] → udp 127.0.0.1:Pf → teled 内 fakeTCP relay (tcpraw.Dial)  ══►
服务端: ══► tele-server 内 fakeTCP relay (tcpraw.Listen :443) → udp 127.0.0.1:Qs → [swgp-server, 可关] → wg0 :51820
```

- relay 是一个很薄的封装（约 200–300 行）：一个 UDP 报文对应一次 `WriteTo`。服务端按对端地址为每个客户端维护一个后端 UDP socket。
- **引入方式**：MIT 许可，采用 **vendor 并固定 commit**（上游是单人维护，约 23 个提交，最近一次提交在 2026-01），必要时在本仓库 `third_party/tcpraw` 维护自己的补丁。
- **需要修补或包一层的已知问题**（实测和阅读源码时发现）：
  1. 进程异常退出时 **iptables 规则会残留**。本次实测两个版本都复现了，已手动清理。对策：启动时按端口扫描并清理残留规则；改用专用链 `TELE-FAKETCP`，退出时整链清空。
  2. 服务端默认监听 `[::]`，在关闭了 IPv6 的机器上会直接失败（本环境复现了）。对策：按地址族分别监听。
  3. 依赖 `iptables` 命令。纯 nft 的系统需要 `iptables-nft` 兼容层；可以考虑改用 `google/nftables` 直接写规则（这是一个小补丁）。
  4. README 的徽章等仍指向 xtaci，属于外观问题。
- **MTU**：相对 UDP 额外开销 = TCP 头 20 + Timestamp 选项 12 − UDP 头 8 = **24 字节**。据此 WG MTU 取值：IPv4 为 1500 − 20 − 32 − 32 = **1416**，IPv6 为 **1396**。启用 swgp paranoid 模式时还要再减去它的开销。teled 自动计算 MTU。

#### 6.4.3 自愈设计（relay 在进程内，比托管子进程更容易做）

1. **探测**（每 5 秒）
   - L1：WG `latest-handshake` 的年龄（> 135s 视为异常）。
   - L2：隧道内对 `tele-server` 健康端点做应用层 ping（带 RTT）。
   - L3：relay 自身的计数器，例如「只发不收」持续时间和最近一次收包时间；以及 TTL 规则是否仍在（防止 firewalld 或 docker 重载时冲掉规则）。
2. **分级动作**（带指数退避和抖动，并做 flap 检测）
   - 规则丢失 → 幂等地重新写入。
   - L3「只发不收」超过阈值，或 L2 连续 3 次失败 → 在**进程内**关闭旧的 tcpraw 连接（会发出 RST），用**新的本地源端口**重新 `Dial`。这会新建一条真实 TCP 握手流，绕开中间设备里卡死的 NAT 或会话状态。WG 会自动在新流上继续工作，因为 WG 的 endpoint 始终是本地 relay 的 UDP 端口，不会变。
   - 连续重拨失败 → 重新解析 endpoint 的 DNS（应对动态 IP），并在备用端口列表之间轮换（例如 443 → 8443 → 自定义端口）。
   - 服务端：tele-server 回收空闲 flow；监听 socket 出错时自动重建。服务端的自愈不依赖客户端能否连上。
   - （可选、默认关闭）**降级**：fake-TCP 持续失败时，临时退回纯 UDP 或 swgp。需要用户显式开启。
3. **可观测**：`tele status` 和 `tele-agent` MCP 的 `list` 输出每层的状态、当前 flow 的四元组、重拨次数、最近一次自愈动作和原因。

**需要在原型中验证**：在真实公网和运营商 NAT 下的吞吐（带 WG，用 iperf3）；长时间运行的稳定性；在 `tc netem` 模拟丢包、乱序以及 NAT 映射过期时的重拨行为。

### 6.5 控制面 / exec 协议

- 运行在 WG 内：`tele-server` 只监听 WG 地址，比如 `10.77.n.2:7070`，并校验对端 IP。WG 本身就是双向公钥认证。可选再加一层会话 token，做纵深防御。
- 协议：gRPC 双向流，或者「TCP + yamux + protobuf 帧」。**推荐 yamux + 自定义帧**：依赖更少，对 stdio 这种字节流更自然，延迟也更低。teled 与每台主机保持一条长连接，本地 shim 通过 unix socket 交给 teled 复用，这样每次命令不用新建 TCP 连接。
- Exec RPC 的语义：argv 或 shell 字符串、cwd、env（**只转发增量**：启动时记录一份基线 env，只转发 Claude 新增或修改的变量，再加一份白名单；`SSH_AUTH_SOCK`、`DISPLAY` 等本地变量不转发）、stdin/stdout/stderr **分流**、退出码与信号、可选 pty、进程组管理、租约心跳（连接断开 → 对远端进程组先 SIGTERM，宽限后再 SIGKILL）。
- 端口转发：HTTP/SSE 类的 MCP 如果 URL 是 `localhost:N`，要么由启动器改写成 WG 地址（服务需监听 0.0.0.0），要么由 teled 提供 `127.0.0.1:N` → 远端 `127.0.0.1:N` 的 TCP 转发（推荐后者，更透明）。

---

## 6B. 替代传输：Shadowsocks + 可恢复会话层（配合 telefs 时推荐）

### 6B.1 为什么可以省掉整条 WG 链路

WG（三层隧道）之所以必要，是因为**内核 NFS 客户端**需要一个 IP 网络。改用 telefs 之后，tele 的所有流量都是**自己进程发出的**：exec RPC、telefs RPC、端口转发，全部是应用层的字节流。这时一个加密的四层流就够了，不再需要三层隧道：

| 旧链路 | 作用 | 换成 SS 之后 |
|---|---|---|
| WireGuard | 加密 + 提供 IP 网络 | SS AEAD 提供加密；不需要 IP 网络 |
| swgp-go | 混淆 WG 特征 | SS 流本身就是全随机字节，没有可识别的握手特征 |
| fake-TCP（tcpraw） | UDP 被封时把 WG 伪装成 TCP | SS 直接跑**真 TCP**，不存在 UDP 被封的问题 |

**连带收益**：两端都**不需要任何特权**（不需要 TUN、raw socket、iptables 或 CAP_NET_RAW）；不需要 MTU 计算；去掉了 AGPL 的 swgp 子进程；也少了单人维护的 tcpraw 依赖。本地只剩「userns + FUSE」这一个内核依赖。

### 6B.2 Shadowsocks 实现选型（已核实许可证）

| 库 | 版本 | 许可证 | SS2022（SIP022） | 说明 |
|---|---|---|---|---|
| [Jigsaw-Code/outline-sdk](https://github.com/Jigsaw-Code/outline-sdk) + [outline-ss-server](https://github.com/Jigsaw-Code/outline-ss-server) | v0.0.23 / v1.9.2 | **Apache-2.0** | 否（只支持 AEAD：chacha20-ietf-poly1305 / aes-gcm） | Outline VPN 的生产实现；自带 salt 重放过滤和抗主动探测；客户端支持连接前缀伪装 |
| shadowsocks/go-shadowsocks2 | v0.1.5 | Apache-2.0 | 否 | 较旧，维护少 |
| sagernet/sing-shadowsocks | v0.2.9 | **GPL-3.0** | 是 | GPL-3.0 与 AGPL-3.0 可以组合，但无必要 |
| **database64128/shadowsocks-go** | v1.15.0 | **AGPL-3.0** | **是** | **已选定**。与 swgp-go 同一作者；`ss2022` 包可以直接当库用；要求 **Go ≥ 1.27** |

**决策（2026-09-28）**：采用 **[database64128/shadowsocks-go](https://github.com/database64128/shadowsocks-go) 的 SS2022（`2022-blake3-aes-256-gcm`）**，**整个项目改为 AGPL-3.0**（仓库 `LICENSE` 已替换为 GNU 官方文本，与上游 LICENSE 的 md5 一致）。**已实测**：用 `ss2022.StreamClientConfig` 和 `StreamServerConfig.NewStreamServer().HandleStream` 在回环上完成加密往返，初始 payload 和双向数据都正确；目标地址字段可以固定为内部名称（例如 `tele.internal:1`），不做通用代理。SS2022 自带时间戳和 salt 重放过滤，因此**要求两端时钟同步**（误差 ≤ 30 秒），install 时需要检查 NTP。

**风险提示**：「全随机字节流」本身在部分审查环境中会被识别（USENIX Security 2023《How the Great Firewall of China Detects and Blocks Fully Encrypted Traffic》）。对策是用 Outline 的前缀伪装功能，或者在外面再套一层 TLS 伪装，作为可选层。如果使用环境没有这类审查，可以忽略。

### 6B.3 能否「所有方面」应对网络重连？——能，但必须自己做会话层

WG 方案里，**短时**断网几乎自动恢复：WG 会漫游，隧道内的 TCP 靠内核重传撑过去。SS 方案里，底层 TCP 一断，里面**所有流立刻失效**。所以必须在 SS 之上、业务之下加一层**可恢复会话层**（思路同 mosh / Eternal Terminal / QUIC 连接迁移）。其实即使用 WG，**长时间**断网（超过内核 TCP 超时，约 15 分钟）也同样需要这一层，所以它无论如何都值得做。

```
exec / telefs / 端口转发 / 失效推送
        │  多路复用流（yamux 或自定义帧）
  ┌─────▼──────────────────────────┐
  │ 可恢复会话层 (session id, 每方向 seq/ack, 重放缓冲)   │  ← 断线重拨后续传，上层无感
  └─────┬──────────────────────────┘
        │  SS AEAD 流（可多条 TCP 连接）
       TCP
```

**会话层机制**：

1. **会话身份**：首次连接时协商 `session_id` 和密钥。重连时，客户端带上 `session_id` 和「已收到的最大 seq」，服务端回复自己的已收 seq，双方从断点**重发未确认的帧**。上层流（包括 MCP 的长连接 stdio）完全无感。
2. **快速断线检测**：应用层心跳每 5 秒一次，15 秒无响应即判定断开；同时监听本机 netlink 的地址和路由变化（Wi-Fi 切换、换 IP），**立即主动重拨**，不必等 TCP 超时。这比 WG 被动等待流量触发要快。
3. **先建后断**：检测到链路质量下降时，先建立新连接，再迁移会话，然后关闭旧连接。
4. **重拨策略**：指数退避加抖动；可以在多个端口或多个 endpoint 之间轮换；重新解析 DNS。
5. **避免队头阻塞**：telefs 的元数据、大块数据、exec stdio 分别走**不同的 TCP 连接**（同属一个会话），避免一个大文件传输拖慢交互操作。

**各业务在断线期间和重连后的语义**：

| 业务 | 断线期间 | 重连后 | 超过租约时限（默认 30 分钟，可配置） |
|---|---|---|---|
| Bash / hooks（exec） | 远端进程**继续运行**，输出写入服务端的有界缓冲（超出部分落盘）；本地 shim 阻塞等待 | 补发缓冲输出，退出码在确认之前一直保留 | 远端进程组先 SIGTERM，再 SIGKILL；shim 返回明确的错误 |
| Claude 发出的中断或超时（SIGTERM/SIGKILL shim） | 信号排队 | 送达远端并执行 | 同上 |
| stdio MCP server | 进程继续存活，JSON-RPC 消息排队 | 续传，Claude 无感 | 进程结束；Claude 显示该 MCP 断开，可以用 `/mcp` 重连 |
| telefs 请求 | 类似 NFS `hard`：请求**阻塞**，不返回错误；`tele-agent` MCP 状态显示「重连中」 | 续传。每个请求带 request id，服务端维护**应答缓存**，保证 create、rename、unlink、append 等非幂等操作**恰好执行一次** | 返回 `EIO`，避免 Claude 永久卡住；阈值可配置 |
| 缓存失效推送 | 事件在服务端排队 | 续传；如果事件缓冲溢出，服务端声明新的 **epoch**，客户端对整棵树做全量失效 | 全量失效 |
| 端口转发（HTTP MCP 等） | TCP 流在会话层之上，行为同 exec | 续传 | 断开 |

**会话层也无法覆盖的情况（非目标，但要显式处理）**：

- **tele-server 进程重启或远端重启**：内存中的会话丢失。
  - telefs 靠**持久句柄**恢复：用 `name_to_handle_at` 或 (dev, ino, generation) 重新打开，然后全量失效缓存。
  - 正在运行的 exec 丢失，Claude 看到命令失败。
  - 可选增强：把命令放进 systemd transient scope（`systemd-run --user --scope`）执行，输出写文件。这样 tele-server 重启后可以重新接管，但复杂度较高，放到后续阶段。
- **本地 tele 启动器崩溃**：FUSE 挂载和 claude 会一起结束，远端进程在租约到期后被清理。用 `tele claude --resume` 可以恢复对话。

### 6B.4 结论

- **能省掉整条链路**：telefs + SS（TCP）+ 可恢复会话层可以替代 WG + swgp + fake-TCP + NFS，而且**两端全程不需要特权**。
- **重连可以覆盖所有「连接层」故障**：断网、换 IP、NAT 超时、服务端短暂不可达都能续传，业务无感，体验比 WG 方案更可控。**进程层**故障（tele-server 重启、主机重启）只能部分恢复，需要在文档中说明。
- **工作量**：会话层 2 周，SS 集成 0.5 周（嵌入 Outline）或 1.5 周（自实现 SIP022），另加断网注入测试：用 `tc netem` 模拟丢包，`iptables` 模拟断流，netns 模拟切换 IP，toxiproxy 做故障注入。
- **取舍**：TCP 在丢包严重的链路上表现不如 UDP 方案（队头阻塞），通过多条连接分担来缓解。如果将来必须走 UDP，会话层保持不变，只需要把下层换成 QUIC（quic-go）。

### 6B.5 客户端单二进制（已实测）

**可以，而且是自然结果**。去掉 NFS 和 WG 以后，本地不再需要 root 守护进程 `teled`：连接、会话层、FUSE 服务都在 `tele claude` 启动器进程内运行。本地只剩一个静态二进制 `tele`，所有角色用 multi-call（按 `argv[0]` 或子命令）分派：

| 角色 | 调用方式 | 说明 |
|---|---|---|
| CLI（`tele host add/ls`、`tele status`） | `tele …` | — |
| 启动器 + 会话 userns 的第 2 阶段（挂载 FUSE、持有 SS 连接和会话层） | `tele claude …`，内部通过 `/proc/self/exe` 再次 exec 自己 | 是 claude 的父进程；shim 通过 unix socket 与它通信 |
| Bash shim | 符号链接 `<会话目录>/bin/bash → tele` | `CLAUDE_CODE_SHELL` 的路径**必须包含 "bash"** |
| hooks / MCP 前缀 | 符号链接 `<会话目录>/bin/tele-exec → tele` | 不能用 `"tele --exec"` 这种带参数的写法：stdio MCP 会把整个 PREFIX 当作可执行文件名去 spawn（见 3.2 节的代码） |
| `rg`（以及可选的 `git`）shim | 符号链接 `<会话目录>/bin/rg → tele` | 要靠 PATH 查找到，所以文件名必须是 `rg` |
| `tele-agent` MCP server | `tele mcp`，写在 `--mcp-config` 里 | — |

符号链接由 `tele claude` 在**运行时**建在会话临时目录里，所以对外分发的仍然只有一个文件。

**实测**（Go 1.27.0，`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`）：把 shadowsocks-go 的 SS2022 客户端和服务端、go-fuse v2.11.0、zap 一起链接进去，得到**静态链接、stripped 的 ELF，大小 4.5 MB**。通过符号链接以 `bash` 为名调用时，能正确分派到 shim。完整功能（会话层、MCP SDK、CLI）加上之后，预计 10–15 MB。

**远端**同样可以是这一个二进制（`tele server …` 子命令），这样发布物只有一个文件。`install_guide` 也就简化为「下载同一个文件，然后执行 `tele server install --pair …`」。

**唯一的外部运行时依赖**：`/dev/fuse` 可访问（常规发行版默认 0666），并且允许非特权 user namespace（Ubuntu 需要随包附带 AppArmor profile，见第 4 节）。

## 7. 本地 MCP：`tele-agent`

由 `tele claude` 通过 `--mcp-config` 注入（stdio，**本地**执行，**不**经过 PREFIX。注入时要对 `tele-agent` 自身豁免：用内部标记让 `tele-exec` 识别并在本地直接执行）。在 Claude 中，下表的工具会显示为 `mcp__tele-agent__list`、`mcp__tele-agent__switch` 等。

> 命名说明：MCP server 叫 `tele-agent`；为避免混淆，远端服务端守护进程命名为 `tele-server`。

| 工具 | 说明 |
|---|---|
| `list` | 列出所有主机，包括 `local`：名称、是否活跃、链路状态、RTT、OS 信息、已导出路径 |
| `switch(host)` | 切换执行和文件的目标主机 |
| `status(host?)` | 详细健康状态（WG 握手、swgp、fake-TCP、NFS、自愈历史） |
| `install_guide(host?)` | 返回在远端安装服务端的步骤和一键命令（见下文） |
| `exec_local(cmd)`（可选） | 在远端模式下，显式在本地执行一次命令 |

MCP server 的 `instructions` 字段告诉模型：当前活跃主机是哪台；Bash 和文件都在该主机上；系统提示里的 OS 信息是本地的，以这里的为准。

**切换语义（需要取舍）**：

- **热切换**：teled 更新会话的「活跃主机」→ shim 下一次调用就发往新主机；再用 `setns` 进入会话的 mount ns（同一用户拥有该 userns，所以有权限），卸载旧 bind、绑定新主机的导出。风险：Claude 主进程自身的 cwd 仍指向旧挂载（lazy umount 之后变成 detached）；已打开的 fd 也还在旧挂载上。只有当新主机上**同样存在**该项目路径时才允许热切换。
- **冷切换**（兜底，最稳）：启动器负责监管 `claude` 进程。`switch` 返回「将在本轮结束后切换」，随后启动器在新命名空间里执行 `claude --resume <session-id>`。由于路径同一，会话键不变，对话历史完整保留。
- 建议：MVP 先做**冷切换** + 「`local` ↔ 远端」切换；热切换放到第二阶段。

**安装指引内容**（`install_guide` 返回，也可以用 `tele host add` 交互完成）：

```bash
# 本地：生成配对串（包含本地 WG 公钥、选定的传输层参数、一次性 token）
tele host add myhost --endpoint 203.0.113.5 --faketcp --swgp zero-overhead-2026
# → 输出: tele-server install --pair 'tele1:....'

# 远端（root）：
curl -fsSL https://github.com/ujzk/tele-agent/releases/latest/download/install.sh | sh
sudo tele-server install --pair 'tele1:....' --export /home/alice/proj --as alice
# → 安装 systemd unit，配置 wg/swgp/faketcp/nfsd/exports，检查 NTP 与防火墙，
#   输出回执串 'tele1r:....'

# 本地：
tele host confirm myhost 'tele1r:....'
```

如果本地有到远端的 SSH，可以提供 `tele host add --ssh user@host` 一步完成（通过 SSH 上传二进制并执行 install）。这样 Claude 在 `local` 模式下也能借助 `install_guide` 的指引自己完成安装。

---

## 8. 风险与缓解

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| R1 | Claude Code 内部行为（cwd 文件、快照、tmp 路径、prefix 覆盖范围）没有文档，可能随版本变化 | **高** | P0 原型；CI 中用 `claude -p` 驱动真实 Claude 跑兼容性测试（在远端执行 `hostname`、写文件后 Read、调用 hook 和 MCP、后台任务、超时中断）；启动时检测版本并告警 |
| R2 | exec 形式的 hooks 可能绕过 `CLAUDE_CODE_SHELL_PREFIX` | 中 | 启动器用 `--setting-sources` + `--settings` 注入改写后的 hooks；插件 hooks 同样处理 |
| R3 | NFS 属性缓存造成读到旧数据或 mtime 误判 | 中 | `actimeo=1`、`lookupcache=positive`、命令结束后定向失效；提供 `noac` 严格模式 |
| R4 | 需要 root（WG、raw socket、iptables、NFS 挂载） | 中 | 特权集中在 `teled` / `tele-server` 两个 systemd 服务；日常使用的 `tele` 不需要 root |
| R5 | 非特权 userns 被 AppArmor 或 sysctl 限制 | 中 | 随包提供 AppArmor profile；退化为 teled 代理 pty 模式 |
| R6 | 整体采用 AGPL-3.0：分发二进制必须提供源码；若把 tele-server 作为网络服务提供给他人使用，也需要向这些用户提供源码 | 低 | 本项目开源，遵守即可；依赖的 BSD、Apache-2.0、MPL-2.0 许可均与 AGPL-3.0 兼容 |
| R7 | fake-TCP 依赖单人维护的 Musixal/tcpraw；TTL 规则残留，或与 docker/firewalld 冲突；中间设备导致流卡死 | 中 | vendor 并固定 commit，自行维护补丁；使用专用 iptables 链，启动时清理残留；自愈时换源端口重拨、轮换端口 |
| R8 | exec 服务本质上是远程代码执行入口 | 高（安全） | 只监听 WG 地址并校验对端 IP；WG 私钥 0600 保存；可选 token；systemd 加固（以目标用户身份执行，不以 root 执行命令） |
| R9 | Claude Code 的 bubblewrap 沙箱会在本地包一层，与 shim 冲突 | 低 | tele 模式下提示关闭沙箱；以后可在远端复现沙箱 |
| R10 | swgp `-2026` 模式要求时钟同步 | 低 | 安装时检查 NTP；teled 报告时钟偏差；可回退旧模式 |

---

## 9. 与替代方案对比

| 方案 | 说明 | 为什么不选 |
|---|---|---|
| SSH 到远端直接运行 claude | 最简单 | 远端要装 Node/Claude 并存放凭证；不能在一个会话里切换主机；不满足「本地 MCP 切换主机」的需求 |
| SSHFS + ssh 命令包装 | 常见做法 | 没有你要求的 WG/混淆/fake-TCP 传输层；SSHFS 的一致性和性能不如 NFSv4.2 |
| 修改 Claude Code（打补丁或 hook Node API） | 可以精确控制 | 二进制是 bun 单文件，打补丁脆弱、难以维护，而且可能违反使用条款 |
| 用 `claude --remote` 类官方远程能力 | — | 语义不同（云端会话），不满足自有主机和自有传输层的需求 |

---

## 10. 实施路线（建议）

**P0 原型验证（约 1 周，决定是否继续）**

1. 不做网络层：用 `unshare` + 本地 bind 模拟「远端」，写一个最小的 `tele-sh` / `tele-exec` / `rg` shim，经 unix socket 由另一个进程执行（模拟远端）。
2. 逐项验证：Bash（含 cd 持久化、后台任务、超时、Ctrl-C 中断）、shell 快照在「远端」生成、shell 形式与 exec 形式的 hooks、stdio MCP、Grep/Glob 走 shim、Read/Write/Edit、`--resume` 冷切换。
3. 产出：一份 Claude Code 行为清单，加上自动化兼容性测试（`claude -p`，可以用 `--max-turns` 和固定提示词驱动）。

**P1 MVP（约 3–4 周）**：`tele-server` exec 服务 + 纯 WG（内核）+ NFSv4.2 + `teled` + `tele claude` + `tele-agent` MCP（list/status/install_guide/冷切换）+ 配对安装。

**P2 传输增强**：swgp-go 子进程托管、fake-TCP（tcpraw）集成与自愈、MTU 自动计算、四种组合的测试矩阵（可以用 netns + `tc netem` 模拟丢包和 NAT 超时）。

**P3 打磨**：热切换、NFS 定向失效、端口转发、AppArmor profile、Go 原生 fake-TCP（可选）、打包（deb/rpm/静态二进制）。

**主要 Go 依赖**：`golang.zx2c4.com/wireguard/wgctrl`、`golang.zx2c4.com/wireguard`（回退）、`github.com/vishvananda/netlink`、`github.com/google/nftables`、`github.com/hashicorp/yamux`、`github.com/creack/pty`、`golang.org/x/sys/unix`、`github.com/modelcontextprotocol/go-sdk`（MCP server）。

---

## 附录 A：tele 启动时注入的环境（草案）

```bash
CLAUDE_CODE_SHELL=/usr/lib/tele/bin/bash            # tele-sh，路径必须含 "bash"
CLAUDE_CODE_SHELL_PREFIX=/usr/lib/tele/bin/tele-exec # hooks + stdio MCP
CLAUDE_CODE_TMPDIR=/var/lib/tele/s/<sid>/tmp         # 两端同路径（NFS）
USE_BUILTIN_RIPGREP=0                                # 使用 PATH 中的 rg shim
PATH=/usr/lib/tele/shim:$PATH                        # rg（可选 git）
TELE_SESSION=<sid>                                   # shim 用它找到 teled 中的会话
# 另加：claude --mcp-config <含 tele-agent 的配置> [--settings <改写后的 hooks>]
```

## 附录 B：未决问题（需要用户决定）

1. 热切换和冷切换哪个优先？（建议先做冷切换）
2. fake-TCP 层已定为 Musixal/tcpraw。是否要保留「降级到纯 UDP」的开关（默认关闭）？
3. ~~swgp-go 是否接受以独立子进程方式分发~~：已改为 AGPL-3.0，可以直接嵌入（仅在保留 WG 后端时相关）。
4. 一个会话是否需要同时挂载多台主机的不同路径（例如 A 的 `/srv/a` 和 B 的 `/srv/b` 同时可见），还是永远只有一台活跃主机？
5. ~~文件层用 telefs 还是 NFS~~ → **已定：telefs**（SS 传输层没有 IP 网络，无法承载 NFS）。
6. ~~传输层~~ → **已定：shadowsocks-go SS2022（TCP）+ 可恢复会话层**；项目许可证改为 AGPL-3.0。待定：WG + swgp + fake-TCP 链路是否保留为可选后端（建议不保留，以降低维护面）。
7. 客户端形态 → **单一静态二进制 `tele`**（multi-call，运行时创建符号链接），远端使用同一个二进制（见 6B.5）。
