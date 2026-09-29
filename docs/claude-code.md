# Claude Code 集成契约

tele 不修改 Claude Code，只通过环境变量、命令行参数和 Claude 的可观察行为接入。下面这些行为**大多没有官方文档**，随时可能随版本变化。每一条都必须有对应的 Claude 兼容性测试（见[测试](coding-standards.md#测试)）：测试失败，说明契约变了，要先更新本文，再改代码。

启动时检测 Claude Code 的版本：不在已验证列表中的版本给出警告，但仍然允许运行。已验证的版本列表以兼容性测试的配置为准。

## CLAUDE_CODE_SHELL

Bash 工具使用的 shell。

- 路径中**必须包含** `bash` 或 `zsh`，并且可执行，否则 Claude 会忽略这个变量。所以 shim 命名为 `<sess>/bin/bash`。
- 调用参数是 `[shell, "-c", "-l", <命令串>]`。命令串的形式是 `source <快照> && <关闭 extglob> && eval '<命令>' && pwd -P >| <cwd 文件>`。
- shell 快照也由这个 shell 生成，所以快照是**在远端**生成的，记录的是远端的 PATH、别名和函数。

## CLAUDE_CODE_SHELL_PREFIX

同时覆盖 Bash、shell 形式的 hooks 和 stdio MCP：

| 调用方 | Claude 的做法 |
|---|---|
| Bash 工具 | 把 `<命令串>` 包装成 `'<PREFIX>' '<命令串>'`，再按 [CLAUDE_CODE_SHELL](#claude_code_shell) 交给 shell |
| shell 形式的 hooks | 把 hook 命令包装成 `'<PREFIX>' '<命令>'`，以 `shell: true` spawn，也就是交给 `/bin/sh -c` |
| stdio MCP server | 直接把**整个 PREFIX 当作可执行文件路径** spawn，唯一的参数是把 `command` 和 `args` 引号拼接而成的 shell 字符串 |

推论：

- PREFIX 不能带参数（例如 `tele --exec`），只能是一个可执行文件的路径，所以 shim 叫 `<sess>/bin/tele-exec`。
- Bash 工具同时受 SHELL 和 PREFIX 影响，会被**包两层**。bash shim 识别出最外层是 tele-exec 的包装后，先去掉这一层。
- 包装时 Claude 会对 PREFIX 中最后一个 ` -` 做特殊处理，所以会话目录和 shim 的路径中不能包含空格。
- 包装使用的引号风格由 Claude 决定（单引号、双引号或反斜杠转义都可能出现），识别包装时要按 POSIX shell 的规则解析。
- **缺口**：exec 形式的 hooks（指定了 `command` 和 `args` 的）直接 spawn，**不经过** PREFIX。对策是启动器用 `--setting-sources` 加 `--settings` 注入改写后的 hooks，插件的 hooks 也要同样处理。

## USE_BUILTIN_RIPGREP 与 git

- 设置 `USE_BUILTIN_RIPGREP=0` 后，Grep 和 Glob 使用 PATH 中的 `rg`，也就是 `rg` shim，搜索在远端的磁盘上执行，不经过 telefs。只有 PATH 中能解析到 `rg` 时 Claude 才这样做；解析不到时会**静默回退**到内置的 ripgrep，不报错。所以 `<sess>/bin/rg` 必须始终存在，兼容性测试要确认 Grep 确实经过了 shim。
- Claude 内部的 git 调用（例如 `status --porcelain`）使用的可执行文件，是**第一次**使用时按 PATH 查到的 `git`，之后在进程内缓存。PATH 中只有 shim 目录，所以查到的是 `git` shim，调用同样转发到远端。

这两项避免了大量元数据操作经过 FUSE 往返。

## scratch 文件

Claude 在本地打开、远端命令也要访问的文件：

| 文件 | 谁写 | 谁读 | 处理 |
|---|---|---|---|
| cwd 文件（`pwd -P >| …`，位于 `CLAUDE_CODE_TMPDIR`） | 远端命令 | 本地 Claude | **回传** |
| shell 快照（`<config>/shell-snapshots/*.sh`） | 远端（生成脚本） | 远端命令 source；本地 Claude 检查它是否存在 | 远端保留一份；回传一份本地副本，用于通过存在性检查 |
| `CLAUDE_ENV_FILE`（`<config>/session-env/…`，由 SessionStart hook 写入） | 远端 hook | 本地 Claude | 回传 |
| `tasks/` 下的标记文件（`echo 0 >| …/tasks/…`） | 远端命令 | 本地 Claude | 回传 |
| 后台任务的输出文件 | Claude 在**本地** `open(path, "w")`，把 fd 作为子进程的 stdout | 本地 Claude | 远端输出经 shim 写入这个本地 fd；它如果位于 scratch 目录中，写入期间不参与同步（见 [scratch 路径改写与回传](exec.md#scratch-路径改写与回传)） |

`<config>` 是 Claude 的配置目录，即 `~/.claude`。机制见 [scratch 路径改写与回传](exec.md#scratch-路径改写与回传)。

**已知限制**：如果用户的命令显式引用后台任务的输出文件（例如 `tail <输出文件路径>`），远端看不到这个文件。Claude 通常用 Read 或 TaskOutput 读取它，影响很小。

## 其它内置行为

| 行为 | 处理 |
|---|---|
| Read、Write、Edit、NotebookEdit，读取图片和 PDF，`@` 引用 | 进程内的文件系统调用 → telefs |
| 启动时加载的 `CLAUDE.md`、`.claude/*`、`.mcp.json` | 项目级的来自远端（telefs）。全局的 `~/.claude/*`、`/etc/claude-code/*` 在本地集合中，来自本地 |
| 设置文件的热加载（Claude 通过文件监视发现设置文件的变化） | 内核只为经过本地 VFS 的操作产生 inotify 事件，telefs 的缓存失效不会产生。所以远端命令对设置文件的改动不会触发热加载，经 Claude 自己的 Write/Edit 做的改动仍然会。影响很小 |
| WebFetch、WebSearch | 在本地或 Anthropic 侧执行，出站 IP 是本地的 |
| 超时与中断：先 SIGTERM，再 SIGKILL（tree-kill，会调用 `ps`） | shim 转发可捕获的信号。SIGKILL 无法捕获，由会话主进程发现 shim 的连接关闭后结束远端进程组（见 [shim 与会话主进程](exec.md#shim-与会话主进程)）。`ps` 走本地 exec 代理，因为它必须看到本地进程 |
| 系统提示词中的操作系统和平台 | Claude 启动时调用 `uname`，而 `uname` 是转发 shim，所以得到的是目标主机的信息。另见[附加系统提示词](#附加系统提示词) |
| 会话存储 `~/.claude/projects/<cwd 编码>` | 以 cwd 路径为键，不同主机上的相同路径会共用会话历史（已知限制，见[启动流程](cli.md#启动流程)） |

## 注入的环境

```bash
# <sess> = /.tele/<sid>（在本地集合中，远端不存在该路径）
PATH=<sess>/bin                                   # 只有 shim（转发到远端的，和本地 exec 代理）
HOME=<远端用户的家目录>                           # ~/.claude* 通过本地集合挂载到这里
USER=<远端用户名>                                 # LOGNAME 同理
SHELL=<sess>/bin/bash                             # 没有 CLAUDE_CODE_SHELL 时 Claude 参考 SHELL
CLAUDE_CODE_SHELL=<sess>/bin/bash
CLAUDE_CODE_SHELL_PREFIX=<sess>/bin/tele-exec
CLAUDE_CODE_TMPDIR=<sess>/tmp                     # 本地 scratch；远端路径由 shim 改写（/tmp 本身是远端的）
USE_BUILTIN_RIPGREP=0
HTTPS_PROXY=http://tele:<密码>@127.0.0.1:<port>   # 本地 CONNECT 代理；HTTP_PROXY 和小写形式同理
NO_PROXY=localhost,127.0.0.1,::1                  # 回环连接不走代理：IDE 插件在本地，远端 MCP 的端口由本地转发
SSL_CERT_FILE=<sess>/ca-bundle.pem                # 本地 CA 合并而成；NODE_EXTRA_CA_CERTS 同样指向它
TELE_SESSION=<sess>                               # shim 据此找到会话主进程的 socket 和会话 token
LD_PRELOAD=<sess>/lib/teleswitch.so               # 与 TELE_SWITCH_FD、TELE_SWITCH_DIR 一起，在视图切换后被清除
```

用户原有环境中指向本地资源的变量（代理、CA、`TMPDIR`、`XDG_RUNTIME_DIR`、`SSH_AUTH_SOCK` 等）不传给 Claude，具体列表以代码为准。

命令行参数：`--append-system-prompt-file <sess>/system-prompt.md`；需要时加 `--setting-sources`、`--settings`（改写后的 hooks）。

上面的变量只对本地的 Claude 进程有意义，shim 不把它们转发到远端（见[环境变量](exec.md#环境变量)）。

## 附加系统提示词

启动时生成 `<sess>/system-prompt.md`，通过 `--append-system-prompt-file` 传入。如果用户自己也传了 `--append-system-prompt` 或 `--append-system-prompt-file`，两段内容**拼接**进同一个文件，不覆盖用户的内容。

找出这两个参数时要遵循 Claude 的命令行解析规则。Claude 用 commander.js 解析命令行：

- 这两个选项都只有长形式，并且必须带值，写作 `--flag value` 或 `--flag=value`；
- 值总是取下一个参数，即使它以 `-` 开头；缺少值是错误；
- `--` 之后的内容都不是选项。

tele 不模拟 Claude 其它选项的参数个数，所以恰好等于这两个选项名的参数一律按选项处理。对不带值的选项，这与 Claude 一致：`-p "--append-system-prompt"` 在 Claude 中同样报「缺少值」，因为 `-p` 不带值，提示词是位置参数。只有紧跟在**带值**选项后面时两者才不同：`--system-prompt "--append-system-prompt"` 在 Claude 中是 `--system-prompt` 的值，tele 却会把它当作选项。只是包含选项名文字的参数（例如提到它的提示词）不受影响。

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

验收标准：Claude 发给 API 的请求中，系统提示词包含目标主机的主机名和发行版，并且不包含本地主机的信息。兼容性测试在模拟 API 一侧检查这一点。

## 验证方法

- **兼容性测试**：用模拟的 Anthropic API 返回事先编排好的 tool_use，驱动真实的 `claude -p`，逐条检查本文的契约。
- **每个新的 Claude Code 版本**在加入已验证列表之前都要重新验证：运行兼容性测试；用 `strace -f` 对切换视图之后的文件访问和 exec 做差异比对。新出现的 dlopen、运行时文件，以及按名字或按绝对路径启动的程序，都要在预加载列表、本地集合或 shim 列表中处理。telefs 在 debug 日志中记录 Claude 进程对疑似运行时文件（`*.so*`、`/etc/ssl` 下的路径）的访问，用来排查这类回归。
- **逆向**：Claude Code 是 bun 编译的单文件二进制，内嵌压缩过的 JS。在二进制中搜索 `CLAUDE_CODE_SHELL_PREFIX`、`USE_BUILTIN_RIPGREP` 等字符串，就能找到相关实现。
- **文件访问**：用 `strace -f -e trace=%file,execve claude -p …` 观察 Claude 在启动、加载配置、发起一次 API 请求期间访问的路径。

## 待验证的行为

以下 Claude Code 和 bun 的行为决定了「先本地加载、再切换视图」能否成立，实现前必须逐条验证。验证通过后把结论作为契约写入上文、由兼容性测试守护，然后删除对应条目。

| 假设 | 不成立时的退路 |
|---|---|
| 在 `main` 之前，bun 仍然是单线程的（`setns(CLONE_NEWNS)` 要求进程不与其它线程共享文件系统信息） | 回到「bind 挂载动态库和 DNS 文件」的做法，本地例外变多 |
| 切换之后，Claude 不再 dlopen 或打开其它本地运行时文件 | 在切换前预先加载；实在不行，加入本地集合 |
| 本地 uid 在远端的 `/etc/passwd` 中可能不存在，但 `os.userInfo()` 等调用不受影响，或者 `USER`、`HOME` 足以兜底。另外，glibc 的 NSS 会按远端的 `nsswitch.conf` dlopen 远端的 `libnss_*.so`，远端 glibc 版本不同时可能崩溃 | 切换前预先加载本地的 NSS 模块，或在远端视图中合成 passwd 条目 |
| Claude 的所有出站 HTTP（API、WebFetch、遥测、OAuth 刷新）都遵循代理。不走代理的连接会在远端视图中做 DNS 解析，从而失败 | 把本地的 DNS 配置文件加入本地集合 |
| 只靠 `SSL_CERT_FILE` 和 `NODE_EXTRA_CA_CERTS`，bun 就会使用 `<sess>/ca-bundle.pem`，不再依赖系统证书目录 | 把本地证书目录 bind 到 `/etc/ssl` 等路径，多一个本地例外 |
| Claude 写 `~/.claude.json` 的方式与 bind 挂载的单个文件兼容。如果它先写临时文件再 rename 覆盖，rename 到挂载点上会失败（`EBUSY` 或 `EXDEV`） | telefs 把远端 `HOME` 下以 `.claude.json` 开头的名字映射到本地文件，让临时文件和目标文件位于同一个文件系统 |
| `tasks/` 标记文件位于某个 scratch 前缀之下 | 为它所在的目录增加 scratch 前缀 |
| Claude 在运行 SessionStart hook 之前，在本地创建了 `CLAUDE_ENV_FILE` 这个文件，而不只是它所在的 `session-env/<id>` 目录。scratch 同步只传普通文件、不传空目录，只有目录时远端 hook 的 `>> "$CLAUDE_ENV_FILE"` 会因远端目录不存在而失败（ENOENT） | 上传时为本会话认领的空目录在远端建出对应目录 |
| Claude 按绝对路径启动的程序可以逐个列出并处理（见 [shim](exec.md#shim)） | 无 |
