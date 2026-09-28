# tele：Claude Code 透明远程执行工具 —— 可行性研究 v2

> 状态：可行性研究已完成，基线已确定，尚未开始实现  
> 日期：2026-09-28  
> 许可证：AGPL-3.0  
> 历史版本（NFS / WireGuard / swgp / fake-TCP 等已放弃方案的完整分析与实测）：[archive/feasibility-v1.md](archive/feasibility-v1.md)

## 0. 结论与已定决策

**结论：可行。** Claude Code 仍在本地运行。远端项目目录通过自研的 FUSE 文件系统 **telefs** 挂到本地的**同一绝对路径**。所有执行类操作（Bash、hooks、stdio MCP、rg、git）通过 Claude Code 官方的环境变量注入点转发到远端。传输层是 **Shadowsocks 2022 over TCP**，上面叠一层**可恢复会话层**。客户端是**单一静态二进制**，**两端都不需要 root 或任何 capability**。Claude Code 本身不需要打补丁。

| # | 决策 | 结果 |
|---|---|---|
| D1 | 文件层 | **telefs**（FUSE + tele 自有通道）；不用 NFS |
| D2 | 传输层 | **database64128/shadowsocks-go 的 SS2022（TCP）+ 可恢复会话层**；不用 WireGuard、swgp、fake-TCP，也**不保留**为可选后端 |
| D3 | 许可证 | 全项目 **AGPL-3.0** |
| D4 | 客户端形态 | **单一静态二进制 `tele`**（multi-call）；远端使用同一个二进制 |
| D5 | 主机切换 | **热切换**（不重启 Claude） |
| D6 | 多主机 | 任意时刻**只有一台活跃主机**，不需要同时挂载多台主机的路径 |
| D7 | 名称 | 本地 MCP server 叫 **`tele-agent`**；远端服务叫 **`tele-server`**（与 `tele server` 子命令对应） |

| 需求 | 可行性 | 实现方式 | 主要风险 |
|---|---|---|---|
| `tele claude` 启动 | ✅ 高 | 启动器：建 userns 和 mountns → 挂载 telefs → 设环境变量 → 启动 `claude` | AppArmor 对 userns 的限制（Ubuntu） |
| Bash 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL` → `bash` shim | 依赖未公开的内部行为（cwd 文件、快照） |
| hooks / stdio MCP 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL_PREFIX` → `tele-exec` shim（已核实同时覆盖两者） | exec 形式的 hook 可能绕过 prefix |
| 文件工具（Read/Write/Edit 等） | ✅ 高 | telefs 同路径挂载；exec 屏障 + 失效推送保证一致性 | 需自研，约 3–4 周 |
| Grep/Glob、Claude 内部的 git 调用 | ✅ 高 | `USE_BUILTIN_RIPGREP=0` + `rg`/`git` shim，在远端执行 | 无 |
| 本地 MCP `tele-agent` | ✅ 高 | `tele mcp`：list / status / switch / install_guide | 热切换的边界情况（第 6 节） |
| 传输与重连 | ✅ 高 | SS2022 + 会话层（续传、心跳、网络变化时主动重拨） | 会话层需自研，约 2 周 |
| 单二进制 | ✅ 已实测 | 4.5 MB 静态探针（SS2022 + go-fuse） | Go ≥ 1.27 |
| 仅 Linux、Go 语言 | ✅ | — | — |

---

## 1. 目标与非目标

**目标**

1. `tele claude [args...]` 与 `claude [args...]` 的体验一致，只是「世界」在远端：Bash、hooks、MCP server 都在远端执行，项目文件也是远端的。
2. 远端**不需要**安装 Node 或 Claude Code，**不存放** Anthropic 凭证。凭证、会话历史和 `~/.claude` 都留在本地。
3. 本地 MCP `tele-agent`：Claude 可以列出主机、查看状态、**热切换**主机（包括 `local`），并能拿到远端服务端的安装指引。
4. 网络断开、切换网络时自动恢复，Claude 侧无感。
5. Go 实现，仅支持 Linux，两端不需要特权。

**非目标**：macOS/Windows；同时挂载多台主机（D6）；多人共享同一个远端会话；远端进程重启后恢复正在运行的命令（见 5.4）；Claude Code 自带的 bubblewrap 沙箱与远程执行的组合。

---

## 2. 总体架构

```
┌──────────────────────────── 本地（普通用户，无特权）─────────────────────────────┐
│ tele claude  (单二进制)                                                          │
│  └─ [userns + mountns]  启动器第 2 阶段 = 会话主进程                              │
│       ├─ telefs FUSE 服务端 ── 挂载于项目根的同一路径（如 /home/alice/proj）       │
│       ├─ 会话层 + SS2022 客户端 ── 每台已连接主机一个会话，其中一台为「活跃」      │
│       ├─ unix socket  ◄── shim 的请求（bash / tele-exec / rg / git）              │
│       └─ claude（原版，子进程）                                                   │
│            ├ Bash    → CLAUDE_CODE_SHELL=<sess>/bin/bash      ─┐                  │
│            ├ hooks   → CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec              │
│            ├ MCP     → 同上（长连接 stdio 代理）                ├─► 会话主进程     │
│            ├ Grep/Glob/git → PATH 中的 <sess>/bin/{rg,git}   ─┘                  │
│            ├ Read/Write/Edit → VFS → telefs                                        │
│            └ MCP "tele-agent" → tele mcp（本地）                                   │
└────────────────────────────────────────┬───────────────────────────────────────┘
                                         │ SS2022 over TCP（多条连接，同属一个会话）
┌────────────────────────────────────────▼───────────────────────────────────────┐
│ tele server（同一个二进制，以目标用户身份运行，无特权，systemd --user 或系统服务）   │
│   SS2022 服务端 → 会话层 → { exec 服务 | telefs 服务端 | 失效推送(inotify) }        │
└─────────────────────────────────────────────────────────────────────────────────┘
```

**单二进制的角色分派**（按 `argv[0]` 或子命令；运行时在会话目录中创建符号链接，对外只分发一个文件）：

| 角色 | 调用方式 | 说明 |
|---|---|---|
| CLI | `tele host add/ls/rm`、`tele status` | — |
| 启动器与会话主进程 | `tele claude …`，内部通过 `/proc/self/exe` 在新的 userns 中重新 exec | claude 的父进程；持有 FUSE、会话和 unix socket |
| Bash shim | `<sess>/bin/bash → tele` | `CLAUDE_CODE_SHELL` 的路径**必须包含 "bash"** |
| hooks / MCP 前缀 | `<sess>/bin/tele-exec → tele` | stdio MCP 把整个 PREFIX 当作可执行文件名直接 spawn（已核实），所以不能写成 `tele --exec` 这种带参数的形式 |
| `rg`、`git` shim | `<sess>/bin/{rg,git} → tele` | 通过 PATH 查找 |
| MCP `tele-agent` | `tele mcp` | 写在 `--mcp-config` 里；本地执行，不经过 PREFIX |
| 远端服务 | `tele server run` / `tele server install --pair …` | — |

**实测**（Go 1.27.0，`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`）：把 shadowsocks-go 的 SS2022 客户端和服务端、go-fuse v2.11.0 链接在一起，得到一个 4.5 MB 的静态、stripped ELF。以 `bash` 为名调用时能正确分派到 shim。功能完整后预计 10–15 MB。

---

## 3. Claude Code 注入点（在 2.1.283 二进制中核实）

对 `/opt/claude-code/bin/claude`（bun 编译的单文件，内嵌压缩 JS）做了字符串层面的逆向。

### 3.1 `CLAUDE_CODE_SHELL`

```js
async function QAn(){let e=a.CLAUDE_CODE_SHELL;
  if(e) if((e.includes("bash")||e.includes("zsh")) && await lbe(e)) return e ...
```

- 路径**必须**包含 `bash` 或 `zsh`，并且可执行。
- 启动参数为 `[shell, "-c", "-l", commandString]`，其中 `commandString` = `source <快照> && <关闭 extglob> && eval '<命令>' && pwd -P >| <cwd 文件>`。
- shell 快照也是用这个 shell 生成的 → 快照在**远端**生成，记录的是远端的 PATH、别名和函数。

### 3.2 `CLAUDE_CODE_SHELL_PREFIX`：覆盖 Bash、shell 形式的 hooks、stdio MCP

```js
// Bash：   Xe = g9(PREFIX, Xe)
// hooks：  Qn = !Xe && !tt && Wt ? g9(Wt, bt) : bt        // spawn 时 shell:true
// MCP：    command = PREFIX || r.command; args = PREFIX ? [qr([r.command, ...r.args])] : r.args
function g9(e,n){ let r=e.lastIndexOf(" -"); ... return `${qr([e])} ${qr([n])}` }
```

- hooks 和 MCP 统一交给 `tele-exec`。它收到一条 shell 字符串，在远端用 `sh -c` 执行，并双向转发 stdio。
- Bash 同时受 SHELL 和 PREFIX 影响，会被**包两层**。bash shim 识别出前缀是 `tele-exec` 后去掉一层。
- **缺口**：hooks 的 exec 形式（`command` + `args`）直接 spawn，**不经过** prefix。对策是启动器通过 `--setting-sources` + `--settings` 注入改写后的 hooks（这两个参数已在 `claude --help` 中确认存在）。

### 3.3 `USE_BUILTIN_RIPGREP`

```js
if (Wo(a.USE_BUILTIN_RIPGREP)) { let {cmd:n} = rm("rg",[]); if(n!=="rg") return {mode:"system",...} }
```

设 `USE_BUILTIN_RIPGREP=0`，Grep/Glob 就会使用 PATH 中的 `rg` shim → 在**远端**本地磁盘上执行。Claude 内部的 git 调用（例如 `status --porcelain`）使用 `gitExecutable ??= which("git") || "git"`（已核实：按 PATH 解析一次后在进程内缓存），所以 PATH 最前面的 `git` shim 会被选中，调用同样转发到远端。缓存的是 shim 的路径，热切换不受影响。这两项避免了大量元数据操作经过 FUSE 往返。

### 3.4 Claude 在本地打开、远端命令也要访问的文件（「scratch」）

| 文件 | 谁写 | 谁读 | 处理 |
|---|---|---|---|
| cwd 文件（`pwd -P >| …`，位于 `CLAUDE_CODE_TMPDIR`） | 远端脚本 | 本地 Claude | **回传**：exec 结束时连同退出码一起返回内容，shim 写入本地 |
| shell 快照（`<config>/shell-snapshots/*.sh`） | 远端（生成脚本） | 远端脚本 source；本地 Claude 检查是否存在 | **按主机保存**：远端保留一份，本地保留副本以通过存在性检查；切换主机时在新主机上**重放生成脚本**（6.2） |
| `CLAUDE_ENV_FILE`（`<config>/session-env/…`，由 SessionStart hook 写入） | 远端 hook | 本地 Claude | 回传 |
| `tasks/` 标记文件（`echo 0 >| …/tasks`） | 远端脚本 | 本地 Claude | 回传 |
| 后台任务输出文件 | Claude 在**本地** `open(path,"w")` 后，以 fd 作为子进程的 stdout（**已核实**） | 本地 | 不需要处理：远端输出经 shim 写入本地 fd |

**机制**：shim 知道三个 scratch 前缀（`CLAUDE_CODE_TMPDIR`、`<config>/shell-snapshots`、`<config>/session-env`）。发给远端的命令字符串和环境变量里，这些路径会被**改写**成远端会话目录 `~/.cache/tele/s/<sid>/…`。前缀由 tele 选定，是带随机数的唯一字符串，不会误匹配。exec 结束时，服务端把该目录中本次**变化过的小文件**（每个 ≤ 1 MB）随退出码一起回传，shim 写回本地。执行前如果本地 scratch 文件有变化，也会上传。

**已知限制**：用户命令里如果显式引用了后台任务输出文件（例如 `tail <output 路径>`），远端看不到这个文件。Claude 通常用 Read 或 TaskOutput 读取它，影响很小。

### 3.5 其它内置行为

| 行为 | 处理 |
|---|---|
| Read/Write/Edit/NotebookEdit、图片和 PDF 读取、`@` 引用 | 进程内 fs 调用 → telefs |
| 启动时加载的 `CLAUDE.md`、`.claude/*`、`.mcp.json` | 经 telefs 读取活跃主机上的文件 |
| WebFetch / WebSearch | 在本地或 Anthropic 侧执行（出网 IP 是本地的） |
| 超时与中断：先 SIGTERM，后 SIGKILL（tree-kill） | shim 转发可捕获的信号。SIGKILL 无法捕获，远端靠「shim 连接关闭 → 结束进程组」兜底，这与网络断线要区分开，见 5.3 |
| 系统提示中的 OS 和平台 | 显示的是本地信息；由 `tele-agent` 的 instructions 以及 switch/list 的结果提供活跃主机的真实信息 |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 路径同一，项目键稳定 |

---

## 4. 路径同一性、命名空间与 telefs

### 4.1 为什么仍然需要挂载

命令虽然都已转发，但 Claude 的文件工具、配置加载、Bash 的 spawn `cwd`、编辑前的 mtime 检查、`/rewind`、`@` 引用都是**本地进程内**的 fs 访问。不挂载就只能禁用内置工具、改用 MCP 替代，那就失去了透明性（详见 v1 的 5A.0）。telefs 只承担这部分**轻量**访问，重负载（搜索、构建、git）都在远端直接执行。

### 4.2 命名空间

`tele claude`（普通用户）用 `CLONE_NEWUSER|CLONE_NEWNS` 重新 exec 自己，uid/gid **映射为自身**，并带上 ambient `CAP_SYS_ADMIN`。第 2 阶段依次执行：`mount --make-rprivate /` → 用 `DirectMountStrict` 把 telefs 挂到项目根 → `stat` 挂载点 → 清除 ambient capability → 启动 `claude`。

项目根在本地不存在、且父目录不可写时，在最近的可写祖先或 `/` 下挂一层 tmpfs 并重建目录骨架。这会遮住本地同名目录，需要在文档中写明。

**本机实测**（Linux 6.18、go-fuse v2.11.0，代码在 [`docs/poc/`](poc/)）：uid 1001 在自己的 userns 中完成挂载、读取和新建文件，宿主机上的属主正确（1001:1002）。遇到的坑及对策：

| 现象 | 原因 | 对策 |
|---|---|---|
| 挂载 EPERM | 测试容器里 `/dev/fuse` 权限为 0600（发行版默认 0666） | install 时检查 |
| 读正常、create 返回 EACCES | FUSE 根 inode 初始属主是 uid 0，在 userns 中未映射（`HAS_UNMAPPED_ID`） | 挂载后立即 `stat` 挂载点 |
| `BACKING_OPEN` EPERM | FUSE passthrough 需要初始命名空间的 CAP_SYS_ADMIN | 不使用 passthrough |
| 子进程仍带 CAP_SYS_ADMIN | ambient capability 会被 exec 继承 | 启动 claude 前 `PR_CAP_AMBIENT_CLEAR_ALL` |

**兼容性**：Ubuntu 23.10+ 的 `apparmor_restrict_unprivileged_userns=1` 需要随包附带 AppArmor profile（授予 `userns,`）；`user.max_user_namespaces=0` 的系统无法使用。

### 4.3 telefs 设计

- **本地**：go-fuse v2（BSD 许可）。inode 表**以路径为键**，并带 (dev, ino, generation) 校验。这样热切换后同一路径的节点 ID 保持不变（6.2）。
- **协议**：FUSE 操作一一映射为 RPC：lookup、getattr、readdirplus、open、read、write、create、mkdir、unlink、rename、symlink、readlink、setattr、fsync、statfs、少量 xattr。每个请求带 request id，服务端维护**应答缓存**，保证非幂等操作恰好执行一次（5.3）。
- **远端**：以目标用户身份直接访问文件，属主天然正确，不需要 uid 映射。
- **一致性**：
  1. **exec 屏障**：远端命令结束时，先推送这期间 inotify 记录到的变更，**再**返回退出码；客户端执行 `NotifyEntry` / `NotifyContent` 之后，shim 才返回。因此「Bash 改了文件 → 紧接着 Read」一定能读到新内容，Claude 的 mtime 检查也是准确的。
  2. **后台变更**（后台任务、MCP、hooks）：通过 inotify 流异步推送。
  3. 有了推送，attr/entry 缓存 TTL 可以设为 30–60 秒；推送断开时退回短 TTL；事件溢出时通过 **epoch** 触发全量失效。
  4. 写入透传，fsync/close 时确认已落盘；不开 writeback cache。
- **inotify 上限**：大仓库需要调高 `fs.inotify.max_user_watches`，由 install 检查。
- **本地主机（`local`）后端**：挂载前先打开项目根的 `O_PATH` fd，通过 `openat` 相对路径提供服务（因为挂载后原目录会被遮住）。
- **远端根目录**：默认只有项目根在远端。Read 访问项目根之外的路径（例如 `/etc/os-release`）读到的是本地文件。`tele-agent` 的 instructions 会明确告知模型这一点：项目外的远端文件请用 Bash 查看。可以通过配置 `extra_roots` 追加远端根目录（仍然只来自活跃主机，不违反 D6）。

---

## 5. 传输与会话层

### 5.1 SS2022

- 库：[database64128/shadowsocks-go](https://github.com/database64128/shadowsocks-go) v1.15.0（AGPL-3.0，要求 Go ≥ 1.27），方法 `2022-blake3-aes-256-gcm`。
- **实测**：`ss2022.StreamClientConfig{…}.NewStreamClient().DialStream` 与 `StreamServerConfig{…}.NewStreamServer().HandleStream` 在回环上完成加密往返；初始 payload 和双向数据都正确。目标地址固定为内部名称（`tele.internal:1`），不做通用代理。
- SS2022 自带时间戳和 salt 重放过滤 → **两端时钟误差必须 ≤ 30 秒**，install 时检查 NTP，`status` 报告时钟偏差。
- 未认证的连接按 RejectPolicy 处理（默认读到超时后关闭，不回任何字节，抵抗主动探测）。
- 风险：「全随机字节流」在部分审查环境中会被识别（USENIX Security 2023）。shadowsocks-go 支持 `UnsafeRequestStreamPrefix` 前缀伪装，作为可选缓解。

### 5.2 可恢复会话层

```
exec / telefs / 失效推送 / MCP 代理 / 端口转发
   │ 多路复用流
┌──▼──────────────────────────────────────────┐
│ 会话层：session_id，每方向 seq/ack，重放缓冲  │ ← 重连后续传，上层无感
└──┬──────────────────────────────────────────┘
   │ SS2022 流 ×N（交互、元数据、大块数据分别走不同连接，避免队头阻塞）
  TCP
```

1. **续传**：重连时双方交换「已收到的最大 seq」，重发未确认的帧。
2. **快速检测**：应用层心跳每 5 秒一次，15 秒无响应判定断开；同时监听本机 netlink 的地址和路由变化，**立即主动重拨**。
3. **先建后断**：链路质量下降时，先建新连接再迁移，然后关闭旧连接。
4. **重拨**：指数退避加抖动，重新解析 DNS，可在多个端口或 endpoint 之间轮换。

### 5.3 各业务的断线语义

| 业务 | 断线期间 | 重连后 | 超过租约（默认 30 分钟） |
|---|---|---|---|
| Bash / hooks | 远端进程**继续运行**，输出缓冲在服务端（有界，超出部分落盘）；shim 阻塞等待 | 补发输出和退出码 | 远端进程组先 SIGTERM 后 SIGKILL；shim 返回错误 |
| shim 被 SIGKILL（Claude 的超时或中断） | 会话主进程检测到 shim 的 unix 连接关闭，**通过会话**通知远端结束进程组 | — | — |
| stdio MCP | 进程存活，消息排队 | 续传 | 进程结束，Claude 显示该 MCP 断开 |
| telefs | 请求阻塞（类似 NFS `hard`）；`tele-agent status` 显示「重连中」 | 续传，应答缓存保证恰好一次 | 返回 `EIO` |
| 失效推送 | 服务端排队 | 续传，溢出时 epoch 全量失效 | 全量失效 |

### 5.4 覆盖不到的情况

- **tele-server 进程重启或远端重启**：会话丢失。telefs 通过持久句柄和全量失效恢复；正在运行的命令失败；MCP server 被重新拉起（6.3 的重放机制同样适用）。
- **本地会话主进程崩溃**：claude 一起退出；远端进程在租约到期后被清理；用 `tele claude --resume` 恢复对话。

---

## 6. `tele-agent` MCP 与热切换

### 6.1 工具

| 工具 | 说明 |
|---|---|
| `list` | 列出所有主机（包括 `local`）：名称、是否活跃、会话状态、RTT、时钟偏差、OS 信息、项目根是否存在 |
| `status(host?)` | 详细状态：会话层、各条连接、重连历史、未完成的命令和后台任务、telefs epoch |
| `switch(host)` | **热切换**活跃主机（6.2） |
| `install_guide(host?)` | 返回远端安装步骤（6.4） |

在 Claude 里显示为 `mcp__tele-agent__list` 等。MCP 的 instructions 会注入：当前活跃主机、它的 OS 信息，以及「系统提示中的平台信息是本地的」「项目外的路径不在远端」这两点说明。

### 6.2 热切换流程

1. **前置检查**：目标主机的会话已建立（未建立则先连接）；项目根在目标主机的**同一路径**存在（D6：不做路径映射，不存在就拒绝，并返回原因和安装或同步建议）；时钟偏差符合要求。
2. **静默**：短暂阻塞新的 telefs 请求，等待正在处理的请求完成（带超时）。
3. **切换 telefs 后端**：切换后端指针，epoch 加 1，对所有已知 inode 和 dentry 发送失效通知。因为 inode 以路径为键，**claude 进程的 cwd 和已缓存的路径依旧有效**，下次访问时会从新主机重新 getattr。在旧后端上打开的文件句柄标记为失效，后续读写返回 `ESTALE`（Claude 的文件操作都是短时打开，影响很小）。
4. **切换执行目标**：之后的 shim 调用发往新主机。**已经在运行的命令和后台任务继续在旧主机上执行直到结束**，输出照常回传（后台输出写的是本地 fd，3.4 已核实）。`status` 会标注它们所在的主机。
5. **shell 快照**：在新主机上重放已记录的快照生成脚本，得到新主机自己的 PATH 和函数；本地副本的路径不变。
6. **stdio MCP 迁移**：`tele-exec` 对 MCP 是一个长连接代理，它缓存了 `initialize` 请求以及 `initialized` 通知。切换时，代理在新主机上启动同一条命令，重放握手（吞掉重复的响应），然后接回数据流。如果工具列表有变化，就向 Claude 发送 `notifications/tools/list_changed`。进行中的请求在旧进程上完成后，再关闭旧进程。MCP server 内部的状态会丢失；可以用 `pin_host` 配置某个 server 不随切换迁移。
7. **hooks**：每次调用都会自动走新主机。
8. **返回结果**：新主机的信息；与切换前相比，`CLAUDE.md` 和 `.claude/settings*.json` 是否有差异（Claude 在启动时已加载配置，**不会热重载**，所以有差异时提示模型）；以及仍在旧主机上运行的任务列表。

**`local` 也是一台主机**：telefs 后端改为本地 `O_PATH` fd，shim 直接在本地（会话命名空间内）执行。

**边界情况**：切换过程中 Claude 可能并行调用工具，所以 switch 会串行化（第 2 步的静默）。旧主机随后断开时，只影响仍在它上面运行的任务，按 5.3 处理。

### 6.3 注入方式

`tele claude` 通过 `--mcp-config` 注入 `tele-agent`（本地 stdio）。`tele-exec` 靠环境标记识别出 `tele-agent` 自己，直接在本地执行，不做转发。

### 6.4 安装与配对

```bash
# 本地：生成配对串（包含 SS2022 PSK、端口、一次性 token）
tele host add myhost --endpoint 203.0.113.5:8443
# → 输出：tele server install --pair 'tele1:…'

# 远端（普通用户即可；systemd --user + loginctl enable-linger，或由管理员安装为系统服务）
curl -fsSLo ~/.local/bin/tele https://github.com/ujzk/tele-agent/releases/latest/download/tele-linux-amd64
chmod +x ~/.local/bin/tele
tele server install --pair 'tele1:…'
# → 检查 NTP、inotify 上限，放通端口；输出回执串 'tele1r:…'

# 本地
tele host confirm myhost 'tele1r:…'
```

本地有到远端的 SSH 时，可以用 `tele host add --ssh user@host` 一步完成。

---

## 7. 风险与缓解

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| R1 | Claude Code 内部行为（cwd 文件、快照、scratch 路径、prefix 覆盖范围）没有文档，会随版本变化 | **高** | P0 原型；CI 中用 `claude -p` 驱动真实 Claude 跑兼容性测试；启动时检测版本并告警 |
| R2 | exec 形式的 hooks 绕过 PREFIX | 中 | `--setting-sources` + `--settings` 注入改写后的 hooks；插件 hooks 同样处理 |
| R3 | 热切换后 Claude 仍使用启动时加载的旧配置 | 中 | switch 结果中报告差异；必要时提示用户 `/resume` |
| R4 | 非特权 userns 被 AppArmor 或 sysctl 限制 | 中 | 随包附带 AppArmor profile；安装时检测并给出指引 |
| R5 | telefs 与会话层都需要自研，POSIX 语义细节多 | 中 | pjdfstest / xfstests 子集，git/npm/cargo 真实负载回归；故障注入（`tc netem`、toxiproxy、netns 切换 IP） |
| R6 | SS2022 的时钟同步要求 | 低 | install 检查 NTP，status 报告时钟偏差 |
| R7 | 全加密流量被审查识别 | 视环境 | 可选前缀伪装；需要时再套一层 TLS 伪装 |
| R8 | exec 服务等于远程代码执行入口 | 高（安全） | SS2022 PSK 认证 + 会话 token；服务以目标用户身份运行；PSK 文件权限 0600 |
| R9 | AGPL-3.0 义务 | 低 | 开源并附带源码；依赖的 BSD、Apache-2.0、MPL-2.0 许可均与之兼容 |
| R10 | Claude 自带的 bubblewrap 沙箱与 shim 冲突 | 低 | tele 模式下提示关闭沙箱 |

---

## 8. 实施路线

| 阶段 | 内容 | 预计 |
|---|---|---|
| **P0 验证** | 本地模拟远端（同机两个进程 + unix socket）：Bash（cd 持久化、后台任务、超时、Ctrl-C）、快照、scratch 改写与回传、两种形式的 hooks、stdio MCP、`rg`/`git` shim、userns + telefs 回环后端、热切换（两个本地目录模拟两台主机）；建立 `claude -p` 兼容性测试 | 1–1.5 周 |
| **P1 MVP** | 单二进制；SS2022 + 会话层（续传、心跳）；exec 服务；telefs（exec 屏障 + 推送）；`tele-agent`（list/status/switch/install_guide）；配对安装 | 5–6 周 |
| **P2 加固** | MCP 迁移重放；先建后断与 netlink 主动重拨；故障注入测试矩阵；telefs 性能（readdirplus、小文件预取）；exec 形式 hooks 的改写 | 3 周 |
| **P3 发布** | AppArmor profile、systemd --user 单元、发布流程（静态二进制、校验和）、文档 | 1–2 周 |

**主要依赖**：`github.com/database64128/shadowsocks-go`（AGPL-3.0）、`github.com/hanwen/go-fuse/v2`（BSD）、`golang.org/x/sys/unix`、`github.com/modelcontextprotocol/go-sdk`（MCP server）、多路复用用 `github.com/hashicorp/yamux`（MPL-2.0）或自研帧。

---

## 附录 A：`tele claude` 注入的环境（草案）

```bash
CLAUDE_CODE_SHELL=<sess>/bin/bash                 # → tele（multi-call）
CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec      # hooks + stdio MCP
CLAUDE_CODE_TMPDIR=/tmp/tele-<random>/tmp          # 本地 scratch；远端路径由 shim 改写
USE_BUILTIN_RIPGREP=0
PATH=<sess>/bin:$PATH                              # rg、git shim
TELE_SOCK=<sess>/sock                              # shim → 会话主进程
# 参数：--mcp-config <含 tele-agent>；必要时 --setting-sources/--settings（改写后的 hooks）
```

## 附录 B：未决问题（次要，有默认值）

1. 快照的本地副本：切换主机后，是否要把本地副本也更新为新主机生成的版本？默认**是**。
2. `git` shim 默认开启（Claude 内部的 git 调用改到远端执行）。有没有需要在本地执行 git 的场景？
3. 远端服务的默认运行方式：`systemd --user`（需要 linger）还是系统服务（需要管理员）？默认 `systemd --user`。
