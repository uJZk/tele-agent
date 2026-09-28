# 总体架构

tele 让 Claude Code 仍在本地运行，但它看到和操作的「世界」在一台远端主机上：Bash、hooks、stdio MCP server 都在远端执行，文件系统也是远端的。Claude Code 本身不打补丁。

## 1. 目标与非目标

**目标**

1. `tele <别名>[:<目录>] [claude 参数...]` 的使用体验与 `claude [参数...]` 一致，区别只在于「世界」在远端：Bash、hooks、MCP server 都在远端执行，**整个文件系统**也是远端的。只有 Claude 自己的配置和凭证等少数路径留在本地（见 [filesystem.md](filesystem.md) 的「本地集合」）。
2. 远端**不需要**安装 Node 或 Claude Code，也**不存放** Anthropic 凭证。凭证、会话历史和 `~/.claude` 都留在本地。
3. 网络断开或切换网络时自动恢复，Claude 侧无感。
4. 用 Go 实现，只支持 Linux，两端都不需要 root 或任何 capability。

**非目标**：会话中切换主机；任何形式的本地 MCP；同时挂载多台主机；macOS 和 Windows；多人共享同一个远端会话；远端服务进程重启后恢复正在运行的命令；Claude Code 自带的 bubblewrap 沙箱与远程执行同时使用（tele 模式下提示用户关闭沙箱）。

## 2. 设计决策

| 决策 | 理由 |
|---|---|
| **文件层用自研的 telefs**（FUSE，走 tele 自己的通道），把远端的整个 `/` 呈现给 Claude 进程 | Claude 的文件工具、配置加载、spawn 的 `cwd`、编辑前的 mtime 检查、`/rewind`、`@` 引用都是**进程内**的文件系统访问，只转发命令不够，必须有真实的挂载（[telefs.md](telefs.md) 第 1 节）。不用 NFS：内核 NFS 客户端需要 root，而且没有服务端推送的缓存失效。 |
| **传输层用 SS2022 over TCP，上面加一层可恢复会话层** | 两端都不需要特权，未认证的连接得不到任何响应（抗主动探测）。TCP 连接一断，里面的流就全部失效，所以断线续传由会话层负责（[transport.md](transport.md)）。 |
| **单一静态二进制 `tele`**（multi-call），远端也用同一个二进制 | 只需分发一个文件。shim 按 `argv[0]` 分派，由会话在运行时创建符号链接。 |
| **一个 tele 进程只对应一台主机**，启动时选定，运行期间不切换；**不提供本地 MCP** | MCP 形式的主机切换可能与 Claude Code 自带的指令冲突，切换后工作目录也难以处理。 |
| **先在本地视图中加载 Claude，再在 `main` 之前切换到远端视图** | 这样 Claude 运行所需的二进制、动态库、DNS 配置都不必作为本地例外留在远端视图里，本地集合可以缩到最小（[filesystem.md](filesystem.md) 第 3 节）。 |
| **系统提示词只写目标主机的信息** | 一个会话只对应一台主机，这些信息在整个会话中都是准确的静态事实（[claude-code.md](claude-code.md) 第 7 节）。 |
| **许可证为 AGPL-3.0** | 依赖的 shadowsocks-go 是 AGPL-3.0。其它依赖（BSD、Apache-2.0、MPL-2.0）都与它兼容。 |

## 3. 结构

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

## 4. 进程与角色

同一个二进制按调用方式分派：

| 角色 | 调用方式 | 说明 |
|---|---|---|
| 命令行 | `tele host …`、`tele doctor` | 见 [cli.md](cli.md) |
| 启动器第 1 阶段 | `tele <别名>[:<目录>] …` | 解析参数，然后通过 `/proc/self/exe` 在新的 userns + mountns 中重新 exec 自己 |
| 会话主进程（启动器第 2 阶段） | 内部重新 exec | Claude 的父进程。持有会话连接、FUSE、shim 的 socket、CONNECT 代理和本地 exec 代理，始终停留在**本地视图** |
| Claude | 会话主进程 exec | 原版 Claude Code，由预加载库 teleswitch 在 `main` 之前切换到**远端视图** |
| shim | `<sess>/bin/{bash,tele-exec,sh,rg,git,uname,…}` → `tele` | 把请求交给会话主进程，见 [exec.md](exec.md) |
| 远端服务 | `tele server run` / `tele server install --pair …` | 远端服务名为 `tele-server` |

`<sess>` 是会话目录。它在本地创建，在远端视图中挂载到 `/.tele/<sid>`（远端不存在这个路径），其中存放 shim、scratch 文件、`CLAUDE_CODE_TMPDIR`、预加载库、本地 CA bundle 和系统提示词文件。

## 5. 各类操作的去向

| Claude 的操作 | 途径 | 在哪里执行 |
|---|---|---|
| Bash 工具 | `CLAUDE_CODE_SHELL` → bash shim | 远端 |
| shell 形式的 hooks、stdio MCP server | `CLAUDE_CODE_SHELL_PREFIX` → tele-exec shim | 远端 |
| Grep、Glob、Claude 内部的 git 调用 | PATH 中的 `rg`、`git` shim | 远端 |
| Read、Write、Edit、NotebookEdit，加载项目配置 | 进程内的文件系统调用 → telefs | 远端文件，经 FUSE 访问 |
| API 请求、WebFetch、OAuth 刷新 | CONNECT 代理 | 本地网络出站 |
| 必须看到本地进程的程序（例如 tree-kill 调用的 `ps`） | 本地 exec 代理 | 本地 |

## 6. 主要依赖

| 依赖 | 用途 | 许可证 |
|---|---|---|
| `github.com/database64128/shadowsocks-go` | SS2022 | AGPL-3.0 |
| `github.com/hanwen/go-fuse/v2` | FUSE | BSD-3-Clause |
| `golang.org/x/sys/unix` | 系统调用 | BSD-3-Clause |

具体版本以 `go.mod` 为准。
