# Claude Code 集成契约

tele 不修改 Claude Code，只通过环境变量、命令行参数和 Claude 的可观察行为接入。下面这些行为**大多没有官方文档**，随时可能随版本变化。每一条都由 Claude 兼容性测试守护（[coding-standards.md](coding-standards.md) 第 11 节）：测试失败，说明契约变了，要先更新本文，再改代码。

启动时检测 Claude Code 的版本：不在已验证列表中的版本给出警告，但仍然允许运行。已验证的版本列表以兼容性测试的配置为准。

## 1. `CLAUDE_CODE_SHELL`：Bash 工具使用的 shell

- 路径中**必须包含** `bash` 或 `zsh`，并且可执行，否则 Claude 会忽略这个变量。所以 shim 命名为 `<sess>/bin/bash`。
- 调用参数是 `[shell, "-c", "-l", <命令串>]`。命令串的形式是 `source <快照> && <关闭 extglob> && eval '<命令>' && pwd -P >| <cwd 文件>`。
- shell 快照也由这个 shell 生成，所以快照是**在远端**生成的，记录的是远端的 PATH、别名和函数。

## 2. `CLAUDE_CODE_SHELL_PREFIX`：覆盖 Bash、shell 形式的 hooks、stdio MCP

| 调用方 | Claude 的做法 |
|---|---|
| Bash 工具 | 把 `<命令串>` 包装成 `'<PREFIX>' '<命令串>'`，再按第 1 节交给 shell |
| shell 形式的 hooks | 把 hook 命令包装成 `'<PREFIX>' '<命令>'`，以 `shell: true` spawn，也就是交给 `/bin/sh -c` |
| stdio MCP server | 直接把**整个 PREFIX 当作可执行文件路径** spawn，唯一的参数是把 `command` 和 `args` 引号拼接而成的 shell 字符串 |

推论：

- PREFIX 不能带参数（例如 `tele --exec`），只能是一个可执行文件的路径，所以 shim 叫 `<sess>/bin/tele-exec`。
- Bash 工具同时受 SHELL 和 PREFIX 影响，会被**包两层**。bash shim 识别出最外层是 tele-exec 的包装后，先去掉这一层。
- 包装时 Claude 会在 PREFIX 中查找最后一个 ` -` 并做特殊处理（具体语义待兼容性测试确认）。所以会话目录和 shim 的路径中不能包含空格。
- **缺口**：exec 形式的 hooks（指定了 `command` 和 `args` 的）直接 spawn，**不经过** PREFIX。对策是启动器用 `--setting-sources` 加 `--settings` 注入改写后的 hooks，插件的 hooks 也要同样处理。

## 3. `USE_BUILTIN_RIPGREP` 与 git

- 设置 `USE_BUILTIN_RIPGREP=0` 后，Grep 和 Glob 使用 PATH 中的 `rg`，也就是 `rg` shim，搜索在远端的磁盘上执行，不经过 telefs。
- Claude 内部的 git 调用（例如 `status --porcelain`）使用的可执行文件，是**第一次**使用时按 PATH 查到的 `git`，之后在进程内缓存。PATH 中只有 shim 目录，所以查到的是 `git` shim，调用同样转发到远端。

这两项避免了大量元数据操作经过 FUSE 往返。

## 4. scratch 文件：Claude 在本地打开、远端命令也要访问

| 文件 | 谁写 | 谁读 | 处理 |
|---|---|---|---|
| cwd 文件（`pwd -P >| …`，位于 `CLAUDE_CODE_TMPDIR`） | 远端命令 | 本地 Claude | **回传** |
| shell 快照（`<config>/shell-snapshots/*.sh`） | 远端（生成脚本） | 远端命令 source；本地 Claude 检查它是否存在 | 远端保留一份；回传一份本地副本，用于通过存在性检查 |
| `CLAUDE_ENV_FILE`（`<config>/session-env/…`，由 SessionStart hook 写入） | 远端 hook | 本地 Claude | 回传 |
| `tasks/` 下的标记文件（`echo 0 >| …/tasks/…`） | 远端命令 | 本地 Claude | 回传 |
| 后台任务的输出文件 | Claude 在**本地** `open(path, "w")`，把 fd 作为子进程的 stdout | 本地 Claude | 无需处理：远端输出经 shim 写入这个本地 fd |

机制（[exec.md](exec.md) 第 5 节）：发给远端的命令串和环境变量中，scratch 路径前缀被改写为远端的会话目录；命令结束时，远端把该目录中本次变化过的小文件随退出码一起回传，shim 写回本地。

**已知限制**：如果用户的命令显式引用后台任务的输出文件（例如 `tail <输出文件路径>`），远端看不到这个文件。Claude 通常用 Read 或 TaskOutput 读取它，影响很小。

## 5. 其它内置行为

| 行为 | 处理 |
|---|---|
| Read、Write、Edit、NotebookEdit，读取图片和 PDF，`@` 引用 | 进程内的文件系统调用 → telefs |
| 启动时加载的 `CLAUDE.md`、`.claude/*`、`.mcp.json` | 项目级的来自远端（telefs）。全局的 `~/.claude/*`、`/etc/claude-code/*` 在本地集合中，来自本地 |
| WebFetch、WebSearch | 在本地或 Anthropic 侧执行，出站 IP 是本地的 |
| 超时与中断：先 SIGTERM，再 SIGKILL（tree-kill，会调用 `ps`） | shim 转发可捕获的信号。SIGKILL 无法捕获，由会话主进程发现 shim 的连接关闭后结束远端进程组（[exec.md](exec.md) 第 4 节）。`ps` 走本地 exec 代理，因为它必须看到本地进程 |
| 系统提示词中的操作系统和平台 | Claude 启动时调用 `uname`，而 `uname` 是转发 shim，所以得到的是目标主机的信息。另见第 7 节 |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 以 cwd 路径为键。不同主机上的相同路径会共用会话历史，这一点已接受（[cli.md](cli.md) 第 2 节） |

## 6. 注入给 Claude 的环境

```bash
# <sess> = /.tele/<sid>（在本地集合中，远端不存在该路径）
CLAUDE_CODE_SHELL=<sess>/bin/bash                # 第 1 节
CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec     # 第 2 节
CLAUDE_CODE_TMPDIR=<sess>/tmp                     # 本地 scratch；远端路径由 shim 改写（/tmp 本身是远端的）
USE_BUILTIN_RIPGREP=0                             # 第 3 节
HTTPS_PROXY=http://127.0.0.1:<port>               # 本地 CONNECT 代理；HTTP_PROXY 同理；用户原有的代理串联在后面
NO_PROXY=localhost,127.0.0.1,::1                  # 回环连接不走代理：IDE 插件在本地，远端 MCP 的端口由本地转发（exec.md 第 7 节）
SSL_CERT_FILE=<sess>/ca-bundle.pem                # 本地 CA 合并而成；NODE_EXTRA_CA_CERTS 同样指向它
LD_PRELOAD=<sess>/lib/teleswitch.so               # main 之前切换到远端视图，切换后从环境中清除
PATH=<sess>/bin                                   # 只有 shim（转发到远端的，和本地 exec 代理）
HOME=<远端用户的家目录>                           # ~/.claude* 通过本地集合挂载到这里
TELE_SOCK=@tele-<sid>                             # 抽象 unix socket：shim → 会话主进程
```

命令行参数：`--append-system-prompt-file <sess>/system-prompt.md`（第 7 节）；需要时加 `--setting-sources`、`--settings`（改写后的 hooks，第 2 节）。

上面的代理、CA、`LD_PRELOAD`、`TELE_*` 变量只对本地的 Claude 进程有意义，shim 不把它们转发到远端（[exec.md](exec.md) 第 3 节）。

## 7. 附加系统提示词

启动时生成 `<sess>/system-prompt.md`，通过 `--append-system-prompt-file` 传入。如果用户自己也传了 `--append-system-prompt` 或 `--append-system-prompt-file`，两段内容**拼接**进同一个文件，不覆盖用户的内容。

内容**只写目标主机的信息**，取值来自建立会话时从服务端获取的信息：

```text
# Target host (tele)

This session operates on the remote host "{{alias}}" via tele.
- Hostname: {{hostname}}
- OS: {{os_pretty_name}} ({{kernel}}, {{arch}})
- User: {{user}} (HOME={{home}}), login shell: {{shell}}
- Working directory: {{workdir}}
```

**有意不写**：tele 的实现细节、本地集合、断线等运行时状态。本地集合带来的例外由 `tele doctor` 和文档说明。

兼容性测试中包含这样的用例：询问「当前系统是什么发行版」「当前主机名是什么」，检查模型的回答与目标主机一致。

## 8. 验证方法

- **兼容性测试**：用模拟的 Anthropic API 返回事先编排好的 tool_use，驱动真实的 `claude -p`，逐条检查本文的契约。
- **逆向**：Claude Code 是 bun 编译的单文件二进制，内嵌压缩过的 JS。在二进制中搜索 `CLAUDE_CODE_SHELL_PREFIX`、`USE_BUILTIN_RIPGREP` 等字符串，就能找到相关实现。
- **文件访问**：用 `strace -f -e trace=%file,execve claude -p …` 观察 Claude 在启动、加载配置、发起一次 API 请求期间访问的路径。
