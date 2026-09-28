# tele：Claude Code 透明远程执行工具 —— 可行性研究

> 状态：可行性研究（未开始实现）  
> 日期：2026-09-28  
> 研究对象：Claude Code 2.1.283（本机二进制逆向核实）、swgp-go v1.10.0、Phantun、Linux 内核 WireGuard / NFS

## 0. 结论摘要

**结论：可行。** 推荐方案是：**Claude Code 仍在本地运行**，通过「路径同一性」（remote 目录挂到本地的**同一绝对路径**）加上 Claude Code 官方提供的执行注入点，把所有**执行类操作**（Bash、hooks、stdio MCP、ripgrep）转发到远端，把所有**文件类操作**（Read/Write/Edit/Glob 的底层 fs 调用）落到 NFS 上。Claude Code 本身不需要打补丁，也不需要改二进制。

| 需求 | 可行性 | 实现方式 | 主要风险 |
|---|---|---|---|
| `tele claude` 启动 | ✅ 高 | 启动器：建挂载命名空间 → 设环境变量 → exec `claude` | 无 |
| Bash 工具远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL` 指向 tele 提供的 shim | 依赖未公开的内部行为（cwd 文件、快照） |
| Hooks 远程执行 | ✅ 高（shell 形式）/ ⚠️ 中（exec 形式） | `CLAUDE_CODE_SHELL_PREFIX` | exec 形式 hook 可能绕过 prefix |
| stdio MCP 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL_PREFIX`（已核实会包裹 stdio MCP） | 无 |
| HTTP/SSE MCP | ⚠️ 中 | 改写 localhost URL 或经隧道端口转发 | 需要改写配置 |
| Read/Write/Edit/Glob/Grep | ✅ 高 | NFS 同路径挂载；`USE_BUILTIN_RIPGREP=0` + 远程 `rg` shim | NFS 属性缓存导致读到旧数据 |
| 本地 MCP `tele`（列出/切换主机、安装说明） | ✅ 高 | Go 写的 stdio MCP server，由启动器注入 | 热切换的边界情况 |
| WireGuard | ✅ 高 | 内核 WG + `wgctrl`（没有内核模块时退回 wireguard-go） | 需要 root |
| swgp-go（可关） | ✅ 高 | **以子进程方式**运行（AGPL-3.0 许可证） | 许可证；`-2026` 模式需要时钟同步 |
| Phantun（可关，自愈） | ✅ 中高 | 第一阶段托管上游二进制并加 supervisor，第二阶段可选 Go 原生重写 | 需要 root 和 iptables/nft；运维面较复杂 |
| NFS 文件层 | ✅ 高 | 内核 nfsd + NFSv4.2，只导出给 WG 对端 IP | 性能、缓存一致性、uid 映射 |
| Go 语言、仅 Linux | ✅ 高 | 整个生态都有成熟 Go 库 | — |

最大的不确定性**不在网络层**，而在 **Claude Code 的内部行为**：临时文件路径、shell 快照、cwd 追踪等都没有公开文档，各版本之间可能变化。因此第一件事是做一个 **P0 原型**，把第 3 节列出的行为逐项跑通，并建立一个**跨 Claude Code 版本的兼容性测试**。

---

## 1. 目标与非目标

**目标**

1. `tele claude [args...]` 与 `claude [args...]` 行为一致，只是「世界」在远端：Bash、hooks、MCP server 都在远端执行，文件都是远端的文件。
2. 远端主机**不需要**安装 Node 或 Claude Code，也**不存放** Anthropic 凭证。凭证、会话历史、`~/.claude` 都留在本地。
3. 本地自动多一个 MCP server `tele`：Claude 可以列出主机、切换主机（包括切回 `local`），并能拿到远端服务端的安装指引。
4. 传输层：WireGuard → [swgp-go，可关] → [Phantun，可关，要求自愈]。
5. 文件层用 NFS。全部用 Go 实现，只支持 Linux。

**非目标**：macOS/Windows、多用户共享同一个远端会话、替代 SSH 做通用远程管理、Claude Code 自带的 bubblewrap 沙箱与远程执行的组合（见第 8 节）。

---

## 2. 总体架构

```
┌──────────────────────── 本地 (Linux) ─────────────────────────┐        ┌──────────────── 远端 (Linux) ────────────────┐
│                                                               │        │                                               │
│  tele claude ──► [userns+mountns]                             │        │  tele-agent (root, systemd)                   │
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
│  teled (root 守护进程): WG / swgp / phantun / NFS 挂载 / 自愈 │        │  wg ◄─ swgp server ◄─ phantun server          │
│   wg0 ─► swgp client ─► phantun client ═══ fake-TCP/UDP ══════╪════════╪══►                                            │
└───────────────────────────────────────────────────────────────┘        └───────────────────────────────────────────────┘
```

**二进制划分（单一 Go 模块，多个入口）**

| 二进制 | 运行位置 | 权限 | 作用 |
|---|---|---|---|
| `tele` | 本地 | 普通用户 | CLI：`tele claude`、`tele host add/ls/rm`、`tele status`；也作为 `tele mcp` 充当 MCP server |
| `teled` | 本地 | root（systemd） | 管理 WG 接口、swgp/phantun 子进程、NFS 挂载、健康检查与自愈；通过 unix socket 接受 `tele` 的请求（用 SO_PEERCRED 鉴权） |
| `tele-sh` / `tele-exec` / `rg` shim | 本地 | 普通用户 | 注入给 Claude Code 的执行垫片，把请求交给 `teled` 复用的多路连接 |
| `tele-agent` | 远端 | root（systemd） | WG 服务端、swgp/phantun 服务端的托管、NFS 导出管理、exec 服务、安装与配对 |

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

**方案**：`tele-agent` 为每个会话在远端创建 `/var/lib/tele/s/<sid>/{tmp,shell-snapshots,session-env}` 并通过 NFS 导出；本地在会话的挂载命名空间里把它们**绑定到同一路径**，并把 `~/.claude/shell-snapshots`、`~/.claude/session-env` bind 到对应子目录。远端执行时，exec 服务也在一个挂载命名空间里做镜像绑定，让 `$HOME/.claude/shell-snapshots` 这个本地路径在远端也能解析到。最简单的做法是让 exec 服务把**本地的 home 路径**映射成同一个 scratch 目录。

快照由 shim 在**远端**生成（因为 `CLAUDE_CODE_SHELL` 也用于生成快照），里面记录的是**远端**的 PATH、别名和函数。这正是我们想要的。

### 3.5 其它内置行为

| 行为 | 处理 |
|---|---|
| Read/Write/Edit/NotebookEdit | 进程内 fs 调用 → 同路径 NFS，天然透明 |
| 内部 `git` 调用（生成 git status 上下文等） | 走 NFS 在本地执行即可；可选：PATH 里放 `git` shim 转发远端，减少 NFS 往返 |
| WebFetch / WebSearch | 在本地或 Anthropic 侧执行，与主机无关（出网 IP 是本地的，需要写进文档） |
| 超时与中断：先 SIGTERM，再 SIGKILL（tree-kill） | shim 转发可捕获的信号；**SIGKILL 无法捕获** → 远端以「连接断开 = 杀进程组」兜底（租约 + 心跳） |
| 后台 Bash（run_in_background） | 同上，输出文件位于共享的 `CLAUDE_CODE_TMPDIR` |
| 系统提示中的 OS 版本、平台 | 显示的是本地信息；由 `tele` MCP 的 instructions 注入「当前主机」的真实信息 |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 路径同一性 → 与在远端直接运行时的项目键一致；切换主机后 `--resume` 仍然可用 |

---

## 4. 路径同一性与命名空间

**原则**：远端目录 `/X` 在本地会话里看到的也是 `/X`。这样 Claude 发出的所有绝对路径（Read 的参数、Bash 的 cwd、hook 的 `CLAUDE_PROJECT_DIR`）不需要任何改写，在两边都成立。

**实现**：

1. `teled`（root）在初始命名空间里把远端导出挂载到 `/run/tele/mnt/<host>/<export-id>`。NFS **不能**在非特权用户命名空间里挂载（没有 `FS_USERNS_MOUNT`），所以这一步必须由特权方完成。
2. `tele claude`（普通用户）执行 `unshare(CLONE_NEWUSER|CLONE_NEWNS)`，把 uid/gid 映射为**自身**（`--map-current-user` 语义），因此 Claude 看到的仍是自己的 uid。在新 userns 中它拥有 CAP_SYS_ADMIN，可以把 `/run/tele/mnt/...` **bind** 到目标路径。bind 已有挂载在 userns 中是允许的。
3. 目标路径在本地不存在、而且父目录不可写时（比如 `/srv/app`），在父目录挂一层 tmpfs 并重建目录骨架，再 bind。这种做法要在文档里写明，因为它会遮住本地同名目录。
4. 完成后 exec `claude`，同时设好环境变量、`--mcp-config`（注入 `tele` MCP）等。

**兼容性注意**：

- Ubuntu 23.10+ 默认开启 `kernel.apparmor_restrict_unprivileged_userns=1` → 需要随包安装 AppArmor profile（`userns,` 权限），或者退化为由 `teled` 代为 fork 进程并通过 pty 代理终端（参考 `machinectl shell`/`run0` 的做法）。
- 一些发行版关闭了 `user.max_user_namespaces` → 同样退化到 `teled` 代理模式。

**uid 映射**：NFS 的 AUTH_SYS 用数值 uid 做鉴权。本地 uid 和远端 uid 往往不同。方案是远端导出时对**该 WG 对端 IP** 使用 `all_squash,anonuid=<远端用户 uid>,anongid=<gid>`，安全边界由 WG 保证。代价只是本地 `ls -l` 显示远端的数字 uid，属于外观问题。

---

## 5. 文件层：NFS

**选型**：远端用内核 nfsd，NFSv4.2（单端口 2049，便于在 WG 上跑；支持服务端 copy 和 sparse）；本地用内核 NFS 客户端。不推荐用户态的 go-nfs：它只支持 v3，性能和一致性也不如内核实现。

**导出**：`tele-agent` 在 `/etc/exports.d/tele.exports` 中管理条目，并调用 `exportfs -ra`：

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

## 6. 传输层

### 6.1 分层与端口链

```
客户端:  wg0(10.77.x.1) → udp 127.0.0.1:Ps ─► swgp-client → udp 127.0.0.1:Pp ─► phantun-client ═fake-TCP═►
服务端:  ═►phantun-server :443/tcp → udp 127.0.0.1:Qs ─► swgp-server → udp 127.0.0.1:51820 ─► wg0(10.77.x.2)
```

两层都可以关掉。关掉某一层时，WG peer 的 endpoint 直接指向下一层（或远端公网地址）。三种组合都要进入测试矩阵：{WG}、{WG+swgp}、{WG+phantun}、{WG+swgp+phantun}。

**拓扑**：本地一个 `tele0` 接口，**每台远端主机一个 peer**，每台主机独立一条 swgp/phantun 进程链。地址按主机分配 `10.77.<n>.0/30`，或者 ULA `fd7e:1e::/64`。AllowedIPs 只包含对端的那个 /32，**不做**全局路由，避免影响本机的其他流量。

### 6.2 WireGuard

- 首选内核 WG（Linux ≥ 5.6），通过 `golang.zx2c4.com/wireguard/wgctrl` 配置，地址和路由用 `vishvananda/netlink`。
- 没有内核模块时退回 `wireguard-go`（可以作为库嵌入，MIT 许可）。
- `PersistentKeepalive=25`，用来维持 NAT 映射和 Phantun 流的状态。

### 6.3 swgp-go（可关）

- v1.10.0，纯 Go，`service.Config` / `Manager` 是**导出的 API**，技术上可以直接嵌入。
- ⚠️ **许可证是 AGPL-3.0**，而本仓库是 MIT。**不要**把它编译进 tele 的二进制。应当作为**独立子进程**分发和托管（聚合分发，不构成衍生作品），由 teled / tele-agent 生成 JSON 配置并监管进程。另一种选择是在安装时从上游 release 下载，并固定 sha256 校验。
- ⚠️ 上游要求 **go ≥ 1.26**（本环境是 1.24.7）。这只影响我们自己构建它的情况。
- 模式：默认用 `zero-overhead-2026`，数据包零开销、**不影响 MTU**；可选 `paranoid-2026`（全包 AEAD 并填充到 MTU，会略微降低 MTU、增加带宽）。**`-2026` 模式带重放保护，要求两端时钟同步**，所以 tele-agent 安装时要检查 NTP，teled 健康检查要报告时钟偏差；时钟不可控时回退到旧版 `zero-overhead`。

### 6.4 Phantun（可关，需要自愈）

**上游概况**：Rust 实现，MIT/Apache-2.0，把 UDP 伪装成 TCP（只做三次握手，之后无状态、无重传），依赖 TUN 和 iptables/nftables（客户端做 MASQUERADE，服务端做 DNAT），需要 root。

**实现选择**：

| 方案 | 优点 | 缺点 |
|---|---|---|
| A. 托管上游二进制（推荐第一阶段） | 立刻可用，协议与上游完全兼容 | 引入非 Go 二进制；自愈只能靠外部监控和重启 |
| B. Go 原生重写 fake-TCP（第二阶段可选） | 单一语言；可以在进程内做精细自愈（换源端口、重握手） | 约 1.5–2.5k 行代码，要自己测抓包兼容性；TUN 用 `wireguard/tun` 或原始套接字 |

**MTU**：Phantun 比 UDP 多 12 字节（TCP 头 20 − UDP 头 8）。上游建议 WG MTU：IPv4 为 1428，IPv6 为 1408。叠加 paranoid 模式时还要再减去 swgp 的开销。teled 根据启用的层**自动计算** MTU，不让用户手填。

**自愈设计（在 teled 中实现，远端 tele-agent 对称实现服务端部分）**：

1. **探测**（每 5 秒）
   - L1：WG `latest-handshake` 的年龄（> 135s 视为异常；正常情况下有流量时 ≤ 120s 会重新握手）。
   - L2：隧道内对 `tele-agent` 健康端点的应用层 ping（带 RTT）。
   - L3：子进程是否存活、TUN 接口和 iptables/nft 规则是否仍在（防止 firewalld 或 docker 重载时冲掉规则）。
2. **分级动作**（带指数退避和抖动，并做 flap 检测）
   - 规则丢失 → 幂等地重新应用规则。
   - L2 连续 3 次失败、但子进程还活着 → **重启 phantun-client，并换一个本地 UDP 源端口**。这会让 Phantun 新建 fake-TCP 流，绕开中间设备里卡死的 NAT 或会话状态（这是 Phantun 最常见的故障模式）。
   - 重启之后仍然失败 → 重新解析 endpoint 的 DNS（应对动态 IP），然后重启整条链（swgp + phantun）。
   - 服务端：tele-agent 定期清理 phantun-server 的空闲连接；隧道长时间没有有效握手时自行重启 phantun-server。服务端的自愈不依赖客户端能否连上。
   - （可选、默认关闭）**降级**：用户允许时，Phantun 持续失败后临时退回纯 UDP 或 swgp。因为用户开 Phantun 往往就是因为 UDP 被封，所以必须显式开启。
3. **可观测**：`tele status` 和 `tele` MCP 的 `list` 输出每层的状态、最近一次自愈动作和原因。

### 6.5 控制面 / exec 协议

- 运行在 WG 内：`tele-agent` 只监听 WG 地址，比如 `10.77.n.2:7070`，并校验对端 IP。WG 本身就是双向公钥认证。可选再加一层会话 token，做纵深防御。
- 协议：gRPC 双向流，或者「TCP + yamux + protobuf 帧」。**推荐 yamux + 自定义帧**：依赖更少，对 stdio 这种字节流更自然，延迟也更低。teled 与每台主机保持一条长连接，本地 shim 通过 unix socket 交给 teled 复用，这样每次命令不用新建 TCP 连接。
- Exec RPC 的语义：argv 或 shell 字符串、cwd、env（**只转发增量**：启动时记录一份基线 env，只转发 Claude 新增或修改的变量，再加一份白名单；`SSH_AUTH_SOCK`、`DISPLAY` 等本地变量不转发）、stdin/stdout/stderr **分流**、退出码与信号、可选 pty、进程组管理、租约心跳（连接断开 → 对远端进程组先 SIGTERM，宽限后再 SIGKILL）。
- 端口转发：HTTP/SSE 类的 MCP 如果 URL 是 `localhost:N`，要么由启动器改写成 WG 地址（服务需监听 0.0.0.0），要么由 teled 提供 `127.0.0.1:N` → 远端 `127.0.0.1:N` 的 TCP 转发（推荐后者，更透明）。

---

## 7. 本地 MCP：`tele`

由 `tele claude` 通过 `--mcp-config` 注入（stdio，**本地**执行，**不**经过 PREFIX。注入时要对 `tele` 自身豁免：用内部标记让 `tele-exec` 识别并在本地直接执行）。

| 工具 | 说明 |
|---|---|
| `tele_list` | 列出所有主机，包括 `local`：名称、是否活跃、链路状态、RTT、OS 信息、已导出路径 |
| `tele_switch(host)` | 切换执行和文件的目标主机 |
| `tele_status(host?)` | 详细健康状态（WG 握手、swgp、phantun、NFS、自愈历史） |
| `tele_install_guide(host?)` | 返回在远端安装服务端的步骤和一键命令（见下文） |
| `tele_exec_local(cmd)`（可选） | 在远端模式下，显式在本地执行一次命令 |

MCP server 的 `instructions` 字段告诉模型：当前活跃主机是哪台；Bash 和文件都在该主机上；系统提示里的 OS 信息是本地的，以这里的为准。

**切换语义（需要取舍）**：

- **热切换**：teled 更新会话的「活跃主机」→ shim 下一次调用就发往新主机；再用 `setns` 进入会话的 mount ns（同一用户拥有该 userns，所以有权限），卸载旧 bind、绑定新主机的导出。风险：Claude 主进程自身的 cwd 仍指向旧挂载（lazy umount 之后变成 detached）；已打开的 fd 也还在旧挂载上。只有当新主机上**同样存在**该项目路径时才允许热切换。
- **冷切换**（兜底，最稳）：启动器负责监管 `claude` 进程。`tele_switch` 返回「将在本轮结束后切换」，随后启动器在新命名空间里执行 `claude --resume <session-id>`。由于路径同一，会话键不变，对话历史完整保留。
- 建议：MVP 先做**冷切换** + 「`local` ↔ 远端」切换；热切换放到第二阶段。

**安装指引内容**（`tele_install_guide` 返回，也可以用 `tele host add` 交互完成）：

```bash
# 本地：生成配对串（包含本地 WG 公钥、选定的传输层参数、一次性 token）
tele host add myhost --endpoint 203.0.113.5 --phantun --swgp zero-overhead-2026
# → 输出: tele-agent install --pair 'tele1:....'

# 远端（root）：
curl -fsSL https://github.com/ujzk/tele-agent/releases/latest/download/install.sh | sh
sudo tele-agent install --pair 'tele1:....' --export /home/alice/proj --as alice
# → 安装 systemd unit，配置 wg/swgp/phantun/nfsd/exports，检查 NTP 与防火墙，
#   输出回执串 'tele1r:....'

# 本地：
tele host confirm myhost 'tele1r:....'
```

如果本地有到远端的 SSH，可以提供 `tele host add --ssh user@host` 一步完成（通过 SSH 上传二进制并执行 install）。这样 Claude 在 `local` 模式下也能借助 `tele_install_guide` 的指引自己完成安装。

---

## 8. 风险与缓解

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| R1 | Claude Code 内部行为（cwd 文件、快照、tmp 路径、prefix 覆盖范围）没有文档，可能随版本变化 | **高** | P0 原型；CI 中用 `claude -p` 驱动真实 Claude 跑兼容性测试（在远端执行 `hostname`、写文件后 Read、调用 hook 和 MCP、后台任务、超时中断）；启动时检测版本并告警 |
| R2 | exec 形式的 hooks 可能绕过 `CLAUDE_CODE_SHELL_PREFIX` | 中 | 启动器用 `--setting-sources` + `--settings` 注入改写后的 hooks；插件 hooks 同样处理 |
| R3 | NFS 属性缓存造成读到旧数据或 mtime 误判 | 中 | `actimeo=1`、`lookupcache=positive`、命令结束后定向失效；提供 `noac` 严格模式 |
| R4 | 需要 root（WG、TUN、iptables、NFS 挂载） | 中 | 特权集中在 `teled` / `tele-agent` 两个 systemd 服务；日常使用的 `tele` 不需要 root |
| R5 | 非特权 userns 被 AppArmor 或 sysctl 限制 | 中 | 随包提供 AppArmor profile；退化为 teled 代理 pty 模式 |
| R6 | swgp-go 是 AGPL-3.0 | 中 | 子进程方式分发、不链接；或让用户自行安装 |
| R7 | Phantun 与 docker、firewalld、nftables 规则冲突；中间设备导致流卡死 | 中 | 专用 nft table 和链、周期性校验；换源端口 + 重启的自愈 |
| R8 | exec 服务本质上是远程代码执行入口 | 高（安全） | 只监听 WG 地址并校验对端 IP；WG 私钥 0600 保存；可选 token；systemd 加固（以目标用户身份执行，不以 root 执行命令） |
| R9 | Claude Code 的 bubblewrap 沙箱会在本地包一层，与 shim 冲突 | 低 | tele 模式下提示关闭沙箱；以后可在远端复现沙箱 |
| R10 | swgp `-2026` 模式要求时钟同步 | 低 | 安装时检查 NTP；teled 报告时钟偏差；可回退旧模式 |

---

## 9. 与替代方案对比

| 方案 | 说明 | 为什么不选 |
|---|---|---|
| SSH 到远端直接运行 claude | 最简单 | 远端要装 Node/Claude 并存放凭证；不能在一个会话里切换主机；不满足「本地 MCP 切换主机」的需求 |
| SSHFS + ssh 命令包装 | 常见做法 | 没有你要求的 WG/混淆/Phantun 传输层；SSHFS 的一致性和性能不如 NFSv4.2 |
| 修改 Claude Code（打补丁或 hook Node API） | 可以精确控制 | 二进制是 bun 单文件，打补丁脆弱、难以维护，而且可能违反使用条款 |
| 用 `claude --remote` 类官方远程能力 | — | 语义不同（云端会话），不满足自有主机和自有传输层的需求 |

---

## 10. 实施路线（建议）

**P0 原型验证（约 1 周，决定是否继续）**

1. 不做网络层：用 `unshare` + 本地 bind 模拟「远端」，写一个最小的 `tele-sh` / `tele-exec` / `rg` shim，经 unix socket 由另一个进程执行（模拟远端）。
2. 逐项验证：Bash（含 cd 持久化、后台任务、超时、Ctrl-C 中断）、shell 快照在「远端」生成、shell 形式与 exec 形式的 hooks、stdio MCP、Grep/Glob 走 shim、Read/Write/Edit、`--resume` 冷切换。
3. 产出：一份 Claude Code 行为清单，加上自动化兼容性测试（`claude -p`，可以用 `--max-turns` 和固定提示词驱动）。

**P1 MVP（约 3–4 周）**：`tele-agent` exec 服务 + 纯 WG（内核）+ NFSv4.2 + `teled` + `tele claude` + `tele` MCP（list/status/install_guide/冷切换）+ 配对安装。

**P2 传输增强**：swgp-go 子进程托管、Phantun 托管与自愈、MTU 自动计算、四种组合的测试矩阵（可以用 netns + `tc netem` 模拟丢包和 NAT 超时）。

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
# 另加：claude --mcp-config <含 tele 的配置> [--settings <改写后的 hooks>]
```

## 附录 B：未决问题（需要用户决定）

1. 热切换和冷切换哪个优先？（建议先做冷切换）
2. Phantun 是托管上游 Rust 二进制，还是直接用 Go 重写？（建议先托管）
3. swgp-go 是否接受以独立子进程方式分发（AGPL 合规）？
4. 一个会话是否需要同时挂载多台主机的不同路径（例如 A 的 `/srv/a` 和 B 的 `/srv/b` 同时可见），还是永远只有一台活跃主机？
