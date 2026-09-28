# tele：Claude Code 透明远程执行工具 —— 可行性研究 v2

> 状态：可行性研究已完成，基线已确定，尚未开始实现  
> 日期：2026-09-28  
> 许可证：AGPL-3.0  
> 历史版本（NFS / WireGuard / swgp / fake-TCP 等已放弃方案的完整分析与实测）：[archive/feasibility-v1.md](archive/feasibility-v1.md)

## 0. 结论与已定决策

**结论：可行。** Claude Code 仍在本地运行。目标主机的**整个根文件系统**通过自研的 FUSE 文件系统 **telefs** 透明代理给 Claude 进程（pivot_root），只有 Claude 自身运行必需的少量路径保留在本地。所有执行类操作（Bash、hooks、stdio MCP、rg、git）通过 Claude Code 官方的环境变量注入点转发到远端。传输层是 **Shadowsocks 2022 over TCP**，上面叠一层**可恢复会话层**。客户端是**单一静态二进制**，**两端都不需要 root 或任何 capability**。Claude Code 本身不需要打补丁。

| # | 决策 | 结果 |
|---|---|---|
| D1 | 文件层 | **telefs**（FUSE + tele 自有通道）；不用 NFS |
| D2 | 传输层 | **database64128/shadowsocks-go 的 SS2022（TCP）+ 可恢复会话层**；不用 WireGuard、swgp、fake-TCP，也**不保留**为可选后端 |
| D3 | 许可证 | 全项目 **AGPL-3.0** |
| D4 | 客户端形态 | **单一静态二进制 `tele`**（multi-call）；远端使用同一个二进制 |
| D5 | 使用方式 | **`tele [选项] <别名>[:<目录>] [claude 参数...]`**：启动时选定一台主机，会话中**不切换**；目录默认为远端用户的家目录 |
| D6 | 本地 MCP | **不提供**：MCP 形式的冷切换和热切换都放弃，因为可能与 Claude Code 自带指令冲突，工作目录的切换也难以处理 |
| D7 | 系统提示词 | `--append-system-prompt-file` **只写目标服务端的信息**（6.5） |
| D8 | 名称 | 远端服务叫 **`tele-server`**（对应 `tele server` 子命令） |

| 需求 | 可行性 | 实现方式 | 主要风险 |
|---|---|---|---|
| `tele <别名>[:<目录>]` 启动 | ✅ 高 | 启动器：建立会话 → 建 userns 和 mountns → 挂载 telefs → 在本地视图中 exec `claude`，`main` 之前切换到远端视图 | AppArmor 对 userns 的限制（Ubuntu） |
| Bash 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL` → `bash` shim | 依赖未公开的内部行为（cwd 文件、快照） |
| hooks / stdio MCP 远程执行 | ✅ 高 | `CLAUDE_CODE_SHELL_PREFIX` → `tele-exec` shim（已核实同时覆盖两者） | exec 形式的 hook 可能绕过 prefix |
| 文件工具（Read/Write/Edit 等） | ✅ 高 | telefs 把目标主机的**整个根文件系统**透明代理给 Claude 进程；Claude 先在本地加载，`main` 之前切换到远端视图，只有 `~/.claude*`、`/etc/claude-code` 等留在本地（第 4 节）；exec 屏障 + 失效推送保证一致性 | 需自研，约 3–4 周；视图切换需 P0 验证（R11） |
| Grep/Glob、Claude 内部的 git 调用 | ✅ 高 | `USE_BUILTIN_RIPGREP=0` + `rg`/`git` shim，在远端执行 | 无 |
| 系统提示词 | ✅ 高 | `--append-system-prompt-file`，只写目标服务端的信息 | 无 |
| 传输与重连 | ✅ 高 | SS2022 + 会话层（续传、心跳、网络变化时主动重拨） | 会话层需自研，约 2 周 |
| 单二进制 | ✅ 已实测 | 4.5 MB 静态探针（SS2022 + go-fuse） | Go ≥ 1.27 |
| 仅 Linux、Go 语言 | ✅ | — | — |

---

## 1. 目标与非目标

**目标**

1. `tele <别名>[:<目录>] [args...]` 与 `claude [args...]` 的体验一致，只是「世界」在远端：Bash、hooks、MCP server 都在远端执行，**整个文件系统**也是远端的（本地集合只剩 `~/.claude*`、`/etc/claude-code` 等，见 4.2）。
2. 远端**不需要**安装 Node 或 Claude Code，**不存放** Anthropic 凭证。凭证、会话历史和 `~/.claude` 都留在本地。
3. 网络断开、切换网络时自动恢复，Claude 侧无感。
4. Go 实现，仅支持 Linux，两端不需要特权。

**非目标**：会话中切换主机，以及任何形式的本地 MCP（D5、D6）；同时挂载多台主机；macOS/Windows；多人共享同一个远端会话；远端进程重启后恢复正在运行的命令（见 5.4）；Claude Code 自带的 bubblewrap 沙箱与远程执行的组合。

---

## 2. 总体架构

```
┌──────────────────────────── 本地（普通用户，无特权）─────────────────────────────┐
│ tele <别名>[:<目录>]  (单二进制)                                                 │
│  └─ [userns + mountns]  启动器第 2 阶段 = 会话主进程                              │
│       ├─ telefs FUSE 服务端 ── 目标主机的整个 / （claude 在 main 前 setns 进入）   │
│       ├─ 会话层 + SS2022 客户端 ── 连接目标主机                                  │
│       ├─ unix socket  ◄── shim 的请求（bash / tele-exec / rg / git）              │
│       └─ claude（原版，子进程）                                                   │
│            ├ Bash    → CLAUDE_CODE_SHELL=<sess>/bin/bash      ─┐                  │
│            ├ hooks   → CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec              │
│            ├ MCP     → 同上（长连接 stdio 代理）                ├─► 会话主进程     │
│            ├ Grep/Glob/git → PATH 中的 <sess>/bin/{rg,git}   ─┘                  │
│            └ Read/Write/Edit → VFS → telefs                                        │
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
| CLI | `tele host add/confirm/ls/rm`、`tele doctor` | — |
| 启动器与会话主进程 | `tele <别名>[:<目录>] …`，内部通过 `/proc/self/exe` 在新的 userns 中重新 exec | claude 的父进程；持有 FUSE、会话和 unix socket |
| Bash shim | `<sess>/bin/bash → tele` | `CLAUDE_CODE_SHELL` 的路径**必须包含 "bash"** |
| hooks / MCP 前缀 | `<sess>/bin/tele-exec → tele` | stdio MCP 把整个 PREFIX 当作可执行文件名直接 spawn（已核实），所以不能写成 `tele --exec` 这种带参数的形式 |
| `rg`、`git` shim | `<sess>/bin/{rg,git} → tele` | 通过 PATH 查找 |
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
| shell 快照（`<config>/shell-snapshots/*.sh`） | 远端（生成脚本） | 远端脚本 source；本地 Claude 检查是否存在 | 远端保留一份，回传一份本地副本以通过存在性检查 |
| `CLAUDE_ENV_FILE`（`<config>/session-env/…`，由 SessionStart hook 写入） | 远端 hook | 本地 Claude | 回传 |
| `tasks/` 标记文件（`echo 0 >| …/tasks`） | 远端脚本 | 本地 Claude | 回传 |
| 后台任务输出文件 | Claude 在**本地** `open(path,"w")` 后，以 fd 作为子进程的 stdout（**已核实**） | 本地 | 不需要处理：远端输出经 shim 写入本地 fd |

**机制**：shim 知道三个 scratch 前缀（`CLAUDE_CODE_TMPDIR`、`<config>/shell-snapshots`、`<config>/session-env`）。发给远端的命令字符串和环境变量里，这些路径会被**改写**成远端会话目录 `~/.cache/tele/s/<sid>/…`。前缀由 tele 选定，是带随机数的唯一字符串，不会误匹配。exec 结束时，服务端把该目录中本次**变化过的小文件**（每个 ≤ 1 MB）随退出码一起回传，shim 写回本地。执行前如果本地 scratch 文件有变化，也会上传。

**已知限制**：用户命令里如果显式引用了后台任务输出文件（例如 `tail <output 路径>`），远端看不到这个文件。Claude 通常用 Read 或 TaskOutput 读取它，影响很小。

### 3.5 其它内置行为

| 行为 | 处理 |
|---|---|
| Read/Write/Edit/NotebookEdit、图片和 PDF 读取、`@` 引用 | 进程内 fs 调用 → telefs |
| 启动时加载的 `CLAUDE.md`、`.claude/*`、`.mcp.json` | 项目级的来自目标主机（telefs）；全局的 `~/.claude/*` 和 `/etc/claude-code/*` 在本地集合中，来自本地 |
| WebFetch / WebSearch | 在本地或 Anthropic 侧执行（出网 IP 是本地的） |
| 超时与中断：先 SIGTERM，后 SIGKILL（tree-kill） | shim 转发可捕获的信号。SIGKILL 无法捕获，远端靠「shim 连接关闭 → 结束进程组」兜底，这与网络断线要区分开，见 5.3 |
| 系统提示中的 OS 和平台 | Claude 启动时通过 `uname` 获取；`uname` 是转发 shim，所以反映的就是目标主机。另有 6.5 的附加提示词 |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 路径同一，项目键稳定 |

---

## 4. 文件系统透明代理：远端根 + 最小本地集合

### 4.1 原则

**Claude 进程看到的整个文件系统（`/`）都是目标主机的**，不只是项目根。`Read /etc/nginx/nginx.conf`、`Read /tmp/out.txt`、`Read ~/.bashrc`、`Read /usr/include/foo.h` 读到的都是远端内容，和远端 Bash 看到的一致。

Claude Code 是本地程序，它启动和运行时需要的文件原本都得来自本机。通过「先在本地加载、再切换到远端视图」、本地 CONNECT 代理和 shim，这些需求大多被消除。最终只剩 **Claude 自己的配置和凭证**（`~/.claude*`）、托管策略（`/etc/claude-code`）以及进程自身要用的 `/proc`、`/sys`、`/dev` 留在本地（下文称为「本地集合」，4.2）。

为什么仍然需要挂载（而不是只转发命令）：Claude 的文件工具、配置加载、spawn `cwd`、编辑前的 mtime 检查、`/rewind`、`@` 引用都是**本地进程内**的 fs 访问（详见 v1 的 5A.0）。

### 4.2 本地集合：先在本地加载，再切换到远端视图

**实测**（Claude Code 2.1.283，`strace -f -e trace=%file,execve claude -p …`，范围覆盖启动、加载配置、一次 API 请求）：Claude 在项目之外访问的路径包括：安装目录（bun 通过 `/proc/self/exe` 读取内嵌 JS）；ld.so、`/etc/ld.so.cache`、libc/libm/libdl/libpthread/librt（运行时还有 libgcc_s 等）；DNS 相关的 `/etc/resolv.conf`、`/etc/hosts`、`/etc/host.conf`、`/etc/nsswitch.conf`；CA 证书目录；`/etc/claude-code/`；`~/.claude*`；以及 exec 的 `git`、`rg`、`uname`、`/bin/sh`。

如果这些路径都作为「本地集合」遮住远端，例外就太多了。所以按下面的办法逐类**消除**：

| 类别 | 消除办法 | 结果 |
|---|---|---|
| Claude 二进制与动态库（ld.so、libc 等） | **先在本地视图中加载，再切换**：Claude 在本地 mountns 中 exec，动态链接器完成所有 `DT_NEEDED` 库的映射之后、`main` 之前，由预加载库把整个进程切换到远端视图（4.4）。之后访问 `/proc/self/exe` 走的是 magic link，与路径无关，所以 bun 读取内嵌 JS 不受影响。会在运行时 dlopen 的库（如 libgcc_s）在切换前预先加载 | 不再需要本地例外 |
| DNS（`resolv.conf`、`hosts`、`nsswitch.conf` 等） | 会话主进程在本地回环上提供 **CONNECT 代理**，并设置 `HTTPS_PROXY`/`HTTP_PROXY=http://127.0.0.1:<port>`。如果用户原本配置了代理，就串联在后面。Claude 自己不再做 DNS 解析，由代理在本地视图中完成 | 不再需要本地例外 |
| CA 证书 | **使用本地的 CA**：Claude 的 TLS 连接经本地代理从本机网络出站，信任关系应当与本地网络一致（例如公司的 HTTPS 中间人 CA）；而远端可能根本没有安装 `ca-certificates`，或者版本很旧。启动时把本地系统 CA（以及用户原有的 `NODE_EXTRA_CA_CERTS`/`SSL_CERT_FILE`）合并为 `/.tele/<sid>/ca-bundle.pem`，并用 `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS` 指向它。**不** bind 到 `/etc/ssl`，所以远端视图中的 `/etc/ssl/certs` 仍然是远端内容 | 使用本地 CA，但不增加本地例外 |
| `git`、`rg`、`uname` | `PATH` 只包含 `/.tele/<sid>/bin` 中的转发 shim | 在远端执行 |
| `/bin/sh`（hooks 的 `shell:true` 固定使用它） | 替换为 tele 的 multi-call `sh`：`-c` 收到的脚本如果是 `'<tele-exec>' '<cmd>'` 形式就直接 exec，否则交给远端的 sh 执行 | 在语义上等同远端 sh |
| 需要本地运行的程序（例如 tree-kill 调用的 `ps`，它必须看到本地进程） | `PATH` 中放**本地 exec 代理**的符号链接：shim 通过 `SCM_RIGHTS` 把自己的 stdio 交给会话主进程，由会话主进程在本地视图中执行真实程序 | 本地执行 |

**最终的本地集合**（在远端视图中通过 bind 挂载可见）：

| 路径 | 原因 | 与远端冲突？ |
|---|---|---|
| `$HOME/.claude/`、`$HOME/.claude.json` | Claude 自己的凭证、设置和会话历史（目标 2） | 仅当远端用户自己也用 Claude Code 时才会遮住远端同名路径 |
| `/etc/claude-code/` | 企业托管策略必须来自本机 | 远端通常不存在 |
| `/proc`、`/sys`、`/dev` | Claude 进程自身要用（`/proc/self` 等） | 实际不构成例外：Agent 通过 Bash 或 Grep/Glob（`rg` shim）访问这些路径，都在远端执行；只有 Read/Write/Edit 直接打开它们时看到的是本地内容，这种用法很少 |
| `/bin/sh` | tele 的转发 sh（见上表） | 语义等同远端 sh |
| `/.tele/<sid>/` | shim、scratch、`CLAUDE_CODE_TMPDIR`、预加载库、本地 CA bundle | 远端不存在该路径 |

`/etc/hosts`、`/etc/resolv.conf`、`/usr/lib/...`、`/lib64/ld-linux...` 等路径在 Claude 看来**都是远端内容**。

**需要 P0 验证的点**：

- `main` 之前 bun 确实是单线程的。`setns(CLONE_NEWNS)` 要求进程不与其它线程共享 fs 结构。
- 切换之后，Claude 是否还会 dlopen 或打开别的本地运行时文件（用 `strace` 对切换后的访问做差异比对）。
- `getpwuid`：本地 uid 在远端 `/etc/passwd` 中可能不存在，要确认 `os.userInfo()` 等调用是否受影响，以及 `USER`/`HOME` 环境变量是否足以兜底。
- Claude 的所有出站 HTTP（API、WebFetch、遥测、OAuth 刷新）都遵循代理；不走代理的出站流量会在远端视图中做 DNS 解析，从而失败。
- 只靠 `SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`，Claude（bun）就会使用 `/.tele/<sid>/ca-bundle.pem`，而不再依赖系统证书目录（strace 显示它会探测 `/etc/ssl/certs` 等目录）。**退路**：把本地证书目录 bind 到 `/etc/ssl` 等路径，代价是多一个本地例外。

### 4.3 HOME

- Claude 进程的 `HOME` 设为**目标主机上远端用户的 home 路径**（例如 `/home/bob`），这样模型写 `~` 时，与远端 Bash 的 `~` 一致。
- 本地的 `~/.claude` 和 `~/.claude.json` bind 挂载到 `$HOME` 下的对应位置（`/home/bob/.claude` → 本地 `/home/alice/.claude`），Claude 通过 `HOME` 找到自己的配置和凭证。

### 4.4 命名空间构建

1. `tele` 用 `CLONE_NEWUSER|CLONE_NEWNS` 重新 exec 自己，uid/gid 映射为自身，并带 ambient `CAP_SYS_ADMIN` → **会话主进程**。它保持**本地视图**，负责 CONNECT 代理和本地 exec 代理，也为 telefs 读取本地文件。
2. 会话主进程在 `/.tele/<sid>/root` 用 `DirectMountStrict` 挂载 telefs（内容是目标主机的 `/`）。telefs 为本地集合中的路径合成挂载点占位节点。
3. **准备远端视图**：一个辅助子进程执行 `unshare(CLONE_NEWNS)`，把本地集合 bind 挂载到占位节点上，rbind `/proc`、`/sys`、`/dev`，`pivot_root` 到 telefs，再 `stat /`（刷新根 inode 属主，见 4.5）。会话主进程通过 `/proc/<pid>/ns/mnt` 持有这个 mountns 的 fd。
4. **启动 Claude**：在**本地视图**中 exec Claude，保留 ambient `CAP_SYS_ADMIN`/`CAP_SYS_CHROOT`，并设置 `LD_PRELOAD=/.tele/<sid>/lib/teleswitch.so`，通过继承的 fd 把远端视图的 mountns 传进去。动态链接器在本地完成所有库的映射。
5. **切换视图**：`teleswitch.so` 的构造函数在 `main` 之前依次执行：预加载运行时库 → `setns(mntns_fd, CLONE_NEWNS)` → `chdir(<工作目录>)` → 关闭 fd → 清除 `LD_PRELOAD` 和 `TELE_*` 环境变量（子进程不会继承）→ `PR_CAP_AMBIENT_CLEAR_ALL` 并通过 capset 清空全部 capability。此后 Claude 以普通权限运行在远端视图中。
6. shim 通过**抽象 unix socket** 与会话主进程通信（不依赖文件路径）。

**关于 `teleswitch.so`**：它必须是 C 写的小共享库（约 100 行）。Go 运行时是多线程的，无法在单线程前提下执行 `setns`。这个库在构建时编译，嵌入 `tele` 二进制，运行时释放到 `/.tele/<sid>/lib/`，所以对外发布仍然只有一个文件（D4）。

### 4.5 FUSE-in-userns 实测

本机实测（Linux 6.18、go-fuse v2.11.0，代码在 [`docs/poc/`](poc/)）：uid 1001 在自己的 userns 中完成挂载、读取和新建文件，宿主机上的属主正确（1001:1002）。遇到的坑及对策：

| 现象 | 原因 | 对策 |
|---|---|---|
| 挂载 EPERM | 测试容器里 `/dev/fuse` 权限为 0600（发行版默认 0666） | install 时检查 |
| 读正常、create 返回 EACCES | FUSE 根 inode 初始属主是 uid 0，在 userns 中未映射（`HAS_UNMAPPED_ID`） | 挂载后立即 `stat` 挂载点 |
| `BACKING_OPEN` EPERM | FUSE passthrough 需要初始命名空间的 CAP_SYS_ADMIN | 不使用 passthrough |
| 子进程仍带 CAP_SYS_ADMIN | ambient capability 会被 exec 继承 | 在 `teleswitch.so` 的构造函数中执行 `PR_CAP_AMBIENT_CLEAR_ALL` 并清空 capability（4.4） |

`pivot_root` 到 FUSE 根、以及在嵌套的 mountns 中做 bind 挂载，是 P0 的验证项。

**兼容性**：Ubuntu 23.10+ 的 `apparmor_restrict_unprivileged_userns=1` 需要随包附带 AppArmor profile（授予 `userns,`）；`user.max_user_namespaces=0` 的系统无法使用。

### 4.6 telefs 设计

- **本地**：go-fuse v2（BSD 许可）。inode 表以 (dev, ino, generation) 标识远端文件，远端重启后可以按路径重新解析（5.4）。本地集合的路径由占位节点覆盖。
- **协议**：FUSE 操作一一映射为 RPC：lookup、getattr、readdirplus、open、read、write、create、mkdir、unlink、rename、symlink、readlink、setattr、fsync、statfs、少量 xattr。每个请求带 request id，服务端维护**应答缓存**，保证非幂等操作恰好执行一次（5.3）。
- **远端**：以目标用户身份访问。用户无权读取的文件（例如 `/etc/shadow`）返回 EACCES，与远端 Bash 的行为一致。`/proc`、`/sys`、`/dev` 不从远端代理，因为本地是真实挂载；远端的进程信息请用 Bash 查看。
- **一致性**：
  1. **exec 屏障**：远端命令结束时，先推送这期间的变更，**再**返回退出码；客户端执行 `NotifyEntry` / `NotifyContent` 后，shim 才返回。这保证「Bash 改了文件 → 紧接着 Read」一定能读到新内容，mtime 检查也准确。
  2. **后台变更**：异步推送。
  3. 有了推送，attr/entry 缓存 TTL 可以设为 30–60 秒；推送断开时退回短 TTL；事件溢出时通过 **epoch** 全量失效。
  4. 写入透传，fsync/close 时确认已落盘；不开 writeback cache。
- **变更监视的范围**：整个根目录无法全部用 inotify 监视。改为**按需注册**：客户端缓存了哪些目录（收到 LOOKUP/READDIR 时），服务端就对这些目录注册 inotify watch；收到 FORGET 时注销。监视数量随工作集增长，而不是随文件系统大小增长。有 root 权限时可以改用 fanotify `FAN_MARK_FILESYSTEM`。仍需调高 `max_user_watches`，由 install 检查。
- **本地主机（`local`）后端**：会话主进程在原始本地视图中直接提供文件，相当于一个 loopback。

---

## 5. 传输与会话层

### 5.1 SS2022

- 库：[database64128/shadowsocks-go](https://github.com/database64128/shadowsocks-go) v1.15.0（AGPL-3.0，要求 Go ≥ 1.27），方法 `2022-blake3-aes-256-gcm`。
- **实测**：`ss2022.StreamClientConfig{…}.NewStreamClient().DialStream` 与 `StreamServerConfig{…}.NewStreamServer().HandleStream` 在回环上完成加密往返；初始 payload 和双向数据都正确。目标地址固定为内部名称（`tele.internal:1`），不做通用代理。
- SS2022 自带时间戳和 salt 重放过滤 → **两端时钟误差必须 ≤ 30 秒**，install 时检查 NTP，`tele host ls` / `tele doctor` 报告时钟偏差。
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
| telefs | 请求阻塞（类似 NFS `hard`）；终端状态栏或日志显示「重连中」 | 续传，应答缓存保证恰好一次 | 返回 `EIO` |
| 失效推送 | 服务端排队 | 续传，溢出时 epoch 全量失效 | 全量失效 |

### 5.4 覆盖不到的情况

- **tele-server 进程重启或远端重启**：会话丢失。telefs 通过持久句柄和全量失效恢复；正在运行的命令失败；stdio MCP server 进程随之结束，Claude 显示该 MCP 断开。
- **本地会话主进程崩溃**：claude 一起退出；远端进程在租约到期后被清理；用 `tele <别名>[:<目录>] --resume` 恢复对话。

---

## 6. 命令行与启动流程

### 6.1 命令形式

```bash
tele [tele 选项] <别名>[:<目录>] [claude 参数...]

tele dev                  # 在远端家目录启动 Claude
tele dev:proj             # ~/proj（相对路径按远端家目录解析）
tele dev:/srv/app         # 绝对路径
tele dev:proj --resume    # 别名之后的参数原样传给 claude
tele dev -p "…"           # 同上
```

- **`<别名>[:<目录>]`**：沿用 scp/rsync 的 `host:path` 写法。目录缺省时是**远端用户的家目录**；相对路径按远端家目录解析；末尾的 `/` 会被规范化掉。目录不存在时直接报错退出，不自动创建。这个目录就是 Claude 的工作目录（cwd，也是 Claude 识别项目的依据）。
- **参数边界**：tele 自己的选项必须写在别名**之前**，别名之后的所有内容都原样传给 Claude Code。这与 `ssh [选项] host [命令]`、`docker run [选项] 镜像 [参数]` 的惯例一致，不需要 `--`，也不会和 claude 的参数冲突。
- **切分规则**：按**第一个** `:` 切分，所以别名中不能含 `:`，目录中可以含。
- **一个 tele 进程只对应一台主机，运行期间不切换**。要换主机，就退出后用另一个别名重新启动。

**其它命令**：

| 命令 | 作用 |
|---|---|
| `tele host add <别名> …` / `tele host confirm` / `tele host ls` / `tele host rm` | 登记主机、配对、查看状态（连通性、RTT、时钟偏差、OS 信息） |
| `tele doctor [别名]` | 本地检查（以及对指定主机的连通性检查），见 6.4 |
| `tele server install/run/uninstall` | 远端服务端 |

`host`、`doctor`、`server`、`help`、`version` 是保留字，不能用作主机别名。

### 6.2 启动流程

1. 解析 `<别名>[:<目录>]`，读取别名配置，建立会话（SS2022 + 会话层）。连接不上时报错退出，并提示运行 `tele doctor <别名>`。
2. 从服务端获取目标信息：hostname、OS/发行版、内核、架构、远端用户、`$HOME`、登录 shell；解析目录并确认它存在。
3. 按第 4 节构建命名空间：挂载 telefs（远端 `/`），准备远端视图（本地集合、`HOME`）。
4. 生成系统提示词文件（6.5）、shim 目录和 `teleswitch.so`，启动 CONNECT 代理，设置好环境变量（附录 A）。
5. 在本地视图中 exec `claude`（别名之后的参数原样透传），由 `teleswitch.so` 在 `main` 之前切换到远端视图并 `chdir` 到工作目录（4.4）。
6. Claude 退出后，清理会话：远端进程按租约规则处理（5.3），卸载 FUSE。

**会话历史不按主机隔离（已接受）**：Claude 以 cwd 路径作为 `~/.claude/projects/` 下的项目键，不同主机上的同一路径会共用会话历史，`--resume` 时会一起列出。这种情况可以接受，tele 不做额外处理。

### 6.3 安装与配对

```bash
# 本地：生成配对串（包含 SS2022 PSK、端口、一次性 token），同时打印远端的安装步骤
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

### 6.4 安装检查与修复策略

**结论：「全部检查 + 按类别处理」**，而不是一律报错或一律自动开启：

- 只影响当前用户、可逆的操作 → **自动执行**；
- 涉及系统范围或需要提权的操作 → **展示将要执行的具体命令，征得同意后执行**；
- 无法修复的问题 → **报错并给出原因和指引**。

理由：

- 一律报错，用户要逐条手动修，安装体验差；
- 一律自动开启，会在用户不知情时改动防火墙、sysctl、linger 这类系统设置，也会在非交互场景（CI、由 Claude 调用）下卡在提权提示上。

**流程**：

1. `tele server install`（以及本地的 `tele doctor`）先做**只读预检**，输出一张清单：✅ 通过 / 🔧 可自动修复 / 🔐 需同意或提权 / ❌ 无法修复 / ⚠️ 警告。
2. 🔧 项自动执行。
3. 🔐 项逐条展示**确切命令**，由用户确认（`[y/N]`）。
4. 最后重新预检，确认结果。
5. 有 ❌ 或未同意的必需项时，以非零状态退出，并打印需要手动执行的命令。

**检查项分类（远端）**：

| 检查项 | 类别 | 处理 |
|---|---|---|
| 二进制与配置目录（`~/.local/bin`、`~/.config/tele`）、PSK 文件权限 0600 | 🔧 自动 | 直接创建和设置 |
| `systemd --user` 单元的安装、启用、启动 | 🔧 自动 | 直接执行；不可用时（没有 user manager）降级为 ⚠️，改为提示前台运行或使用系统服务 |
| linger（`loginctl enable-linger $USER`） | 🔐 同意 | 没有 linger 时用户登出后服务会停止。polkit 的 `set-self-linger` 在活跃会话中通常允许，SSH 等非活跃会话可能需要认证。拒绝时降级为 ⚠️，并说明后果 |
| 监听端口可达（本机防火墙：firewalld/ufw/nft） | 🔐 同意 | 生成对应前端的放行命令，经同意后用 sudo 执行；云安全组无法检测，只给出提示 |
| `fs.inotify.max_user_watches` 不足（按项目文件数估算） | 🔐 同意 | 写入 `/etc/sysctl.d/90-tele.conf` 并 `sysctl --system`；拒绝时降级为 ⚠️：失效推送会退回短 TTL |
| 时钟同步（SS2022 要求误差 ≤ 30 秒） | 误差已超限为 ❌；NTP 未启用为 🔐 | 启用 `timedatectl set-ntp true` 需要同意；当前误差已超限时直接报错，因为连接会被拒绝 |
| 内核版本、`/proc/sys/fs/inotify` 可用 | ❌ | 报错并说明最低要求 |

**检查项分类（本地，`tele doctor`，首次运行 `tele <别名> claude` 时自动执行）**：

| 检查项 | 类别 | 处理 |
|---|---|---|
| `/dev/fuse` 存在且可读写 | 权限不足为 🔐；不存在为 ❌ | 发行版默认 0666；异常时给出 `modprobe fuse` 或 udev 规则的建议 |
| 非特权 userns 可用（`max_user_namespaces`、Ubuntu 的 AppArmor 限制） | 🔐 同意 | 安装随包附带的 AppArmor profile（需要 sudo），而不是全局关闭限制；拒绝时报错 |
| Claude Code 版本在兼容列表中 | ⚠️ | 未验证的版本给出警告，但仍允许运行（R1） |
| 与目标主机的时钟偏差 | ⚠️ / ❌ | 同上 |

**通用约定**：

- **非交互**（没有 TTY，例如在 CI 中，或由 Claude 在 Bash 里调用）时**绝不提权**：🔧 项照常执行，🔐 项全部视为未同意，打印需要手动执行的命令。
- `--yes` 用于显式同意所有 🔐 项，适合自动化部署；`--check` 只做预检；`--print-commands` 只打印命令、不执行。
- 所有改动写入清单文件 `~/.config/tele/install-manifest.json`，`tele server uninstall` 据此逐项回滚。
- 每一步都是幂等的，可以重复运行。

---

### 6.5 附加系统提示词：目标服务端信息

**注入方式**：启动时生成 `<sess>/system-prompt.md`，通过 `--append-system-prompt-file` 传入（`claude --help` 中已确认存在）。如果用户自己也传了 `--append-system-prompt[-file]`，两段内容**拼接**进同一个文件，不覆盖用户的内容。

**内容**：**只写这台目标服务端的信息**。一个会话只对应一台主机，所以这些都是静态事实，整个会话中保持准确。取值来自 6.2 第 2 步从服务端获取的信息：

```text
# Target host (tele)

This session operates on the remote host "{{alias}}" via tele.
- Hostname: {{hostname}}
- OS: {{os_pretty_name}} ({{kernel}}, {{arch}})
- User: {{user}} (HOME={{home}}), login shell: {{shell}}
- Working directory: {{workdir}}
```

**有意不写的内容**：tele 的实现细节、本地集合（4.2）、网络断线等运行时状态。本地集合的例外由 `tele doctor` 和文档说明（R12）。

**兼容性测试**（纳入 R1 的 `claude -p` 测试集）：用「当前系统是什么发行版？」「当前主机名是什么？」这类提示，检查模型的回答与目标服务端一致。

## 7. 风险与缓解

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| R1 | Claude Code 内部行为（cwd 文件、快照、scratch 路径、prefix 覆盖范围）没有文档，会随版本变化 | **高** | P0 原型；CI 中用 `claude -p` 驱动真实 Claude 跑兼容性测试；启动时检测版本并告警 |
| R2 | exec 形式的 hooks 绕过 PREFIX | 中 | `--setting-sources` + `--settings` 注入改写后的 hooks；插件 hooks 同样处理 |
| R3 | 不同主机上的同一路径共用 Claude 的项目键，会话历史混在一起 | 低 | **已接受**，不处理（6.2） |
| R4 | 非特权 userns 被 AppArmor 或 sysctl 限制 | 中 | 随包附带 AppArmor profile；安装时检测并给出指引 |
| R5 | telefs 与会话层都需要自研，POSIX 语义细节多 | 中 | pjdfstest / xfstests 子集，git/npm/cargo 真实负载回归；故障注入（`tc netem`、toxiproxy、netns 切换 IP） |
| R6 | SS2022 的时钟同步要求 | 低 | install 检查 NTP，`tele host ls` / `tele doctor` 报告时钟偏差 |
| R7 | 全加密流量被审查识别 | 视环境 | 可选前缀伪装；需要时再套一层 TLS 伪装 |
| R8 | exec 服务等于远程代码执行入口 | 高（安全） | SS2022 PSK 认证 + 会话 token；服务以目标用户身份运行；PSK 文件权限 0600 |
| R9 | AGPL-3.0 义务 | 低 | 开源并附带源码；依赖的 BSD、Apache-2.0、MPL-2.0 许可均与之兼容 |
| R10 | Claude 自带的 bubblewrap 沙箱与 shim 冲突 | 低 | tele 模式下提示关闭沙箱 |
| R11 | 「本地加载、再切换视图」不成立或不完整：`main` 之前 bun 已经多线程，导致 setns 失败；或者切换后 Claude 还会 dlopen、打开本地运行时文件；或者有出站流量不走代理 | **高** | P0 首先验证；每个 Claude Code 版本都用 strace 对切换后的访问做差异比对；预先加载运行时库；telefs 记录 Claude 进程对疑似运行时文件（`*.so*`、`/etc/ssl` 等）的访问供排查。**退路**：回到「bind 挂载动态库和 DNS 文件」的做法，代价是本地例外变多 |
| R12 | 本地集合遮住远端同名路径（`~/.claude*`、`/etc/claude-code`） | 低 | 只有远端用户自己也使用 Claude Code 时才会冲突；`/proc`、`/sys`、`/dev` 实际通过 Bash 在远端访问 |

---

## 8. 实施路线

| 阶段 | 内容 | 预计 |
|---|---|---|
| **P0 验证** | 本地模拟远端（同机两个进程 + unix socket）：**`teleswitch.so`：main 前 setns 切换视图（最优先验证）**、pivot_root 到 FUSE 根 + 嵌套 mountns 的 bind 挂载、CONNECT 代理覆盖所有出站流量、切换后的 strace 差异、本地 CA bundle 是否生效、本地 exec 代理、`/bin/sh` 替换、HOME 映射、`getpwuid`；Bash（cd 持久化、后台任务、超时、Ctrl-C）、快照、scratch 改写与回传、两种形式的 hooks、stdio MCP、`rg`/`git` shim、userns + telefs 回环后端；建立 `claude -p` 兼容性测试 | 1–1.5 周 |
| **P1 MVP** | 单二进制；SS2022 + 会话层（续传、心跳）；exec 服务；telefs（exec 屏障 + 推送）；`tele <别名>[:<目录>]` 命令行与 `tele host`/`tele doctor`；系统提示词生成；配对安装 | 5–6 周 |
| **P2 加固** | 先建后断与 netlink 主动重拨；故障注入测试矩阵；telefs 性能（readdirplus、小文件预取）；exec 形式 hooks 的改写 | 3 周 |
| **P3 发布** | AppArmor profile、systemd --user 单元、发布流程（静态二进制、校验和）、文档 | 1–2 周 |

**主要依赖**：`github.com/database64128/shadowsocks-go`（AGPL-3.0）、`github.com/hanwen/go-fuse/v2`（BSD）、`golang.org/x/sys/unix`、多路复用用 `github.com/hashicorp/yamux`（MPL-2.0）或自研帧。

---

## 附录 A：注入给 Claude 的环境（草案）

```bash
# <sess> = /.tele/<sid>（本地集合，远端不存在该路径）
CLAUDE_CODE_SHELL=<sess>/bin/bash                 # → tele（multi-call）
CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec      # hooks + stdio MCP
CLAUDE_CODE_TMPDIR=<sess>/tmp                      # 本地 scratch；远端路径由 shim 改写（/tmp 本身是远端的）
USE_BUILTIN_RIPGREP=0
HTTPS_PROXY=http://127.0.0.1:<port>                # 本地 CONNECT 代理（4.2）；HTTP_PROXY 同理；用户原有代理串联在后
SSL_CERT_FILE=<sess>/ca-bundle.pem                 # 本地 CA 合并而成（4.2）；NODE_EXTRA_CA_CERTS 同样指向它
LD_PRELOAD=<sess>/lib/teleswitch.so                # main 前切换到远端视图，随后从环境中清除（4.4）
PATH=<sess>/bin                                    # 只有 shim：bash/rg/git/uname/sh 转发远端，ps 等为本地 exec 代理（4.2）
HOME=<目标主机上的远端 home>                       # ~/.claude* 在本地集合中挂到这里（4.3）
TELE_SOCK=@tele-<sid>                              # 抽象 unix socket：shim → 会话主进程
# 参数：--append-system-prompt-file <sess>/system-prompt.md（6.5，只含目标服务端信息，与用户自带的内容拼接）；
#       必要时 --setting-sources/--settings（改写后的 hooks）
```

## 附录 B：已确认的次要决策

1. `git` shim 默认开启（Claude 内部的 git 调用在远端执行）。
2. 远端服务默认以 `systemd --user` 运行；linger 等系统级设置按 6.4 的「检查 → 自动 / 征得同意 / 报错」策略处理。
3. 放弃本地 MCP 和主机切换；一个 `tele` 会话对应一台主机（D5、D6）。
