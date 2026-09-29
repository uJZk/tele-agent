# 总体架构

tele 让 Claude Code 仍在本地运行，但它看到和操作的「世界」在一台远端主机上：Bash、hooks、stdio MCP server 都在远端执行，文件系统也是远端的。

## 目标与非目标

**目标**

1. `tele <别名>[:<目录>] [claude 参数...]` 的使用体验与 `claude [参数...]` 一致，区别只在于「世界」在远端：Bash、hooks、MCP server 都在远端执行，**整个文件系统**也是远端的。只有 Claude 自己的配置和凭证等少数路径留在本地（见[本地集合](filesystem.md#本地集合)）。
2. 远端**不需要**安装 Node 或 Claude Code，也**不存放** Anthropic 凭证。凭证、会话历史和 `~/.claude` 都留在本地。
3. 网络断开或切换网络时自动恢复，Claude 侧无感。
4. 用 Go 实现，只支持 Linux，两端都不需要 root 或任何 capability。

**非目标**

- 会话中切换主机；同时挂载多台主机。
- tele 自己的 MCP server（例如用来切换主机）。
- 替代 SSH 做通用的远程管理：tele 只为 Claude Code 服务，不提供交互式 shell，也不做通用代理。
- macOS 和 Windows。
- 多人共享同一个远端会话。
- 远端服务进程重启后恢复正在运行的命令。
- 与 Claude Code 自带的 bubblewrap 沙箱同时使用：沙箱在本地包裹的是 shim，命令实际在远端执行，沙箱约束不到远端命令，反而可能破坏 shim（例如沙箱隔离了网络命名空间后，shim 连不上抽象 unix socket）。tele 模式下提示用户关闭沙箱。

## 设计决策

| 决策 | 理由 |
|---|---|
| **不修改 Claude Code，也不拦截它的函数或系统调用** | Claude Code 是 bun 编译的单文件二进制，打补丁或 hook 其内部 API 脆弱、难以跟随版本维护，还可能违反使用条款；用 LD_PRELOAD、ptrace 或 seccomp 拦截文件系统调用（proot 式）同样脆弱，而且有性能代价。teleswitch 只在 `main` 之前做一次视图切换，不拦截任何函数。 |
| **文件层用自研的 telefs**（FUSE，走 tele 自己的通道），把远端的整个 `/` 呈现给 Claude 进程 | Claude 的文件工具、配置加载、spawn 的 `cwd`、编辑前的 mtime 检查、`/rewind`、`@` 引用都是**进程内**的文件系统访问，只转发命令不够，必须有真实的挂载（见[为什么需要挂载](telefs.md#为什么需要挂载)）。不用 NFS：内核 NFS 客户端需要 root，而且没有服务端推送的缓存失效。 |
| **传输层用 SS2022 over TCP，上面加一层可恢复会话层** | 两端都不需要特权，未认证的连接得不到任何响应（抗主动探测）。tele 的所有流量都是自己进程内的应用层字节流，所以一条加密的四层流就够了，不需要三层隧道。TCP 连接一断，里面的流就全部失效，所以断线续传由会话层负责（见 [transport.md](transport.md)）。 |
| **单一静态二进制 `tele`**（multi-call），远端也用同一个二进制 | 只需分发一个文件。shim 按 `argv[0]` 分派，由会话在运行时创建符号链接。 |
| **一个 tele 进程只对应一台主机**，启动时选定，运行期间不切换 | 用 MCP 在会话中切换主机，可能与 Claude Code 自带的指令冲突，切换后工作目录也难以处理。 |
| **先在本地视图中加载 Claude，再在 `main` 之前切换到远端视图** | 这样 Claude 运行所需的二进制、动态库、DNS 配置都不必作为本地例外留在远端视图里，本地集合可以缩到最小（见[本地集合](filesystem.md#本地集合)）。 |
| **系统提示词只写目标主机的信息** | 一个会话只对应一台主机，这些信息在整个会话中都是准确的静态事实（见[附加系统提示词](claude-code.md#附加系统提示词)）。 |
| **许可证为 AGPL-3.0** | 依赖的 shadowsocks-go 是 AGPL-3.0，整个项目因此采用 AGPL-3.0。新增依赖的许可证要求见[依赖](coding-standards.md#依赖)。 |

**AGPL-3.0 的义务**：发布二进制时必须同时提供对应的完整源码；修改过的 tele server 如果供他人通过网络使用，也必须向这些用户提供修改后的源码（AGPL-3.0 第 13 条）。所以每个发布都附带源码归档，其中 vendor 了全部 Go 依赖，离线也能构建出同样的二进制（`make dist`）。

## 结构

```
┌─────────────────────────── 本地（普通用户，无特权）───────────────────────────┐
│ tele <别名>[:<目录>]                                                          │
│  └─ [userns + mountns] 会话主进程（启动器第 2 阶段）                           │
│       ├─ telefs FUSE 服务端 ── 远端的整个 /                                    │
│       ├─ 会话层 + SS2022 客户端 ── 连接目标主机                                │
│       ├─ 抽象 unix socket ◄── shim 的请求                                     │
│       ├─ CONNECT 代理（127.0.0.1）── Claude 的出站 HTTPS                       │
│       └─ claude（原版，子进程；main 之前切换到远端视图）                       │
│            ├ Bash          → CLAUDE_CODE_SHELL        = <sess>/bin/bash       │
│            ├ hooks、MCP    → CLAUDE_CODE_SHELL_PREFIX = <sess>/bin/tele-exec  │
│            ├ Grep/Glob/git → PATH 中的 <sess>/bin/{rg,git}                    │
│            └ Read/Write/Edit 等 → 内核 VFS → telefs                            │
└──────────────────────────────────────┬───────────────────────────────────────┘
                                       │ SS2022 over TCP（多条连接，同属一个会话）
┌──────────────────────────────────────▼───────────────────────────────────────┐
│ tele server（同一个二进制，以目标用户身份运行，无特权）                        │
│   SS2022 服务端 → 会话层 → { exec 服务 | telefs 服务 | 变更推送（inotify）}     │
└──────────────────────────────────────────────────────────────────────────────┘
```

## 进程与角色

同一个二进制按调用方式分派：

| 角色 | 调用方式 | 说明 |
|---|---|---|
| 命令行 | `tele host …`、`tele doctor` | 见 [cli.md](cli.md) |
| 启动器第 1 阶段 | `tele <别名>[:<目录>] …` | 解析参数，然后通过 `/proc/self/exe` 在新的 userns + mountns 中重新 exec 自己 |
| 会话主进程（启动器第 2 阶段） | 内部重新 exec | Claude 的父进程。持有会话连接、FUSE、shim 的 socket、CONNECT 代理和本地 exec 代理，始终停留在**本地视图** |
| 视图辅助进程 | 会话主进程内部 exec | 在新的 mountns 中准备远端视图，然后退出（见[命名空间的构建](filesystem.md#命名空间的构建)） |
| 启动阶段 | 会话主进程内部 exec | 在新的 mountns 中把 `/proc` 盖成空的，然后 exec Claude（见[命名空间的构建](filesystem.md#命名空间的构建)） |
| Claude | 启动阶段 exec | 原版 Claude Code，由预加载库 teleswitch 在 `main` 之前切换到**远端视图** |
| shim | `<sess>/bin/{bash,tele-exec,sh,rg,git,uname,…}` → `tele` | 把请求交给会话主进程，见 [exec.md](exec.md) |
| 远端服务 | `tele server run` / `tele server install --pair …` | 远端服务名为 `tele-server` |

内部角色（会话主进程、视图辅助进程、启动阶段）按 `argv[0]` 分派，名字形如 `tele:session`，含有 shim 名中不会出现的 `:`。

`<sess>` 是会话目录。它在本地创建，在远端视图中挂载到 `/.tele/<sid>`（远端不存在这个路径）。其中存放 shim、`CLAUDE_CODE_TMPDIR`、预加载库、会话 token、本地 CA bundle、系统提示词文件和会话主进程的日志（shim 出错时让用户去看它，见 [shim 与会话主进程](exec.md#shim-与会话主进程)）。

## 各类操作的去向

| Claude 的操作 | 途径 | 在哪里执行 |
|---|---|---|
| Bash 工具 | `CLAUDE_CODE_SHELL` → bash shim | 远端 |
| shell 形式的 hooks、stdio MCP server | `CLAUDE_CODE_SHELL_PREFIX` → tele-exec shim | 远端 |
| Grep、Glob、Claude 内部的 git 调用 | PATH 中的 `rg`、`git` shim | 远端 |
| Read、Write、Edit、NotebookEdit，加载项目配置 | 进程内的文件系统调用 → telefs | 远端文件，经 FUSE 访问 |
| API 请求、WebFetch、OAuth 刷新 | CONNECT 代理 | 本地网络出站 |
| 必须看到本地进程的程序（例如 tree-kill 调用的 `ps`） | 本地 exec 代理 | 本地 |

所有 stdio MCP server 都在远端运行，包括本地 `~/.claude.json` 中配置的。
