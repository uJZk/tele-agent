# Claude Code 集成契约

tele 不修改 Claude Code，只通过环境变量、命令行参数和 Claude 的可观察行为接入。下面这些行为**大多没有官方文档**，随时可能随版本变化。每一条都必须有对应的 Claude 兼容性测试（见[测试](coding-standards.md#测试)）：测试失败，说明契约变了，要先更新本文，再改代码。

启动时检测 Claude Code 的版本：不在已验证列表中的版本给出警告，但仍然允许运行。已验证的版本列表是 `claudever.Verified`；兼容性测试中有一条检查被测版本在列表中，所以只有全部兼容性测试都通过的版本才能加入。

## CLAUDE_CODE_SHELL

Bash 工具使用的 shell。

- 路径中**必须包含** `bash` 或 `zsh`，并且可执行，否则 Claude 会忽略这个变量。所以 shim 命名为 `<sess>/bin/bash`。
- Bash 工具的调用参数是 `[shell, "-c", <命令串>]`，不是登录 shell。命令串的形式是 `source <快照> 2>/dev/null || true && <关闭 extglob> && <取消名为 unsetenv 的别名和函数> && eval '<命令>' < /dev/null && pwd -P >| <cwd 文件>`。cwd 文件位于 `CLAUDE_CODE_TMPDIR`，名为 `claude-<随机>-cwd`。
- 命令的 stdin 是 `/dev/null`，所以 Bash 工具的命令从不读取 Claude 的 stdin。
- Claude 读取 cwd 文件后以它作为下一条命令的工作目录；它如果在项目目录之外，Claude 把工作目录重置回项目根。
- shell 快照也由这个 shell 生成，调用参数是 `[shell, "-c", "-l", <脚本>]`，即登录 shell。所以快照是**在远端**生成的，记录的是远端的 PATH、别名和函数。
- 快照中定义了同名函数，把 `rg`、`find`、`grep` 转给 Claude 二进制内嵌的实现，二进制的路径取自 `CLAUDE_CODE_EXECPATH`，或 Claude 的默认安装路径。这些路径在远端通常不可执行，函数随即回退到 `command rg` 等远端程序。

## CLAUDE_CODE_SHELL_PREFIX

同时覆盖 Bash、shell 形式的 hooks 和 stdio MCP：

| 调用方 | Claude 的做法 |
|---|---|
| Bash 工具 | 把 `<命令串>` 包装成 `<PREFIX> '<命令串>'` 两个 shell 词，再按 [CLAUDE_CODE_SHELL](#claude_code_shell) 交给 shell |
| shell 形式的 hooks | 把 hook 命令包装成 `<PREFIX> '<命令>'`，以 `shell: true` spawn，也就是 `/bin/sh -c "<PREFIX> '<命令>'"`，PREFIX 收到的唯一参数就是 hook 命令 |
| stdio MCP server | 直接把**整个 PREFIX 当作可执行文件路径** spawn，唯一的参数是把 `command` 和 `args` 引号拼接而成的 shell 字符串 |

推论：

- PREFIX 不能带参数（例如 `tele --exec`），只能是一个可执行文件的路径，所以 shim 叫 `<sess>/bin/tele-exec`。
- Bash 工具同时受 SHELL 和 PREFIX 影响，会被**包两层**。bash shim 识别出最外层是 tele-exec 的包装后，先去掉这一层。
- 包装时 Claude 会对 PREFIX 中最后一个 ` -` 做特殊处理，所以会话目录和 shim 的路径中不能包含空格。
- 包装使用的引号风格由 Claude 决定（单引号、双引号或反斜杠转义都可能出现），识别包装时要按 POSIX shell 的规则解析。
- 例子中 PREFIX 没有引号，因为路径中没有需要引用的字符；命令串中的单引号写成 `'"'"'`。
- **缺口**：exec 形式的 hooks（指定了 `command` 和 `args` 的）直接 spawn，**不经过** PREFIX。对策是启动器用 `--setting-sources` 加 `--settings` 注入改写后的 hooks，插件的 hooks 也要同样处理。

## USE_BUILTIN_RIPGREP 与 git

- 设置 `USE_BUILTIN_RIPGREP=0` 后，Grep 和 Glob 使用 PATH 中的 `rg`，也就是 `rg` shim，搜索在远端的磁盘上执行，不经过 telefs。只有 PATH 中能解析到 `rg` 时 Claude 才这样做；解析不到时会**静默回退**到内置的 ripgrep，不报错。所以 `<sess>/bin/rg` 必须始终存在，兼容性测试要确认 Grep 确实经过了 shim。
- Claude 内部的 git 调用（例如 `status --porcelain`）使用的可执行文件，是**第一次**使用时按 PATH 查到的 `git`，之后在进程内缓存。PATH 中只有 shim 目录，所以查到的是 `git` shim，调用同样转发到远端。

这两项避免了大量元数据操作经过 FUSE 往返。

Claude 自己也调用 `rg`：启动时先执行 `rg --version`，并用 `rg --files` 列出工作目录，以及 `~/.claude/plugins/cache` 下的 `.orphaned_at` 标记文件（用来清理孤立的插件）。在 tele 下，这些调用同样经 shim 在远端执行，而 `~/.claude` 属于本地集合，所以插件缓存的扫描看到的是远端的同名目录，通常不存在，结果为空。空结果只是不清理任何插件，没有危害。

## 代理与 CA

- API 请求遵循 `HTTPS_PROXY`：经 CONNECT 隧道到达 API 主机，Claude 自己不解析 API 的主机名。
- 代理 URL 中的用户名和密码，Claude 作为 Basic `Proxy-Authorization` 发送，所以 tele 把 CONNECT 代理的随机密码放在 `HTTPS_PROXY` 的 userinfo 中。
- `SSL_CERT_FILE` 或 `NODE_EXTRA_CA_CERTS` 任意一个指向的 CA 都会被信任。tele 两个都设置，指向同一个 bundle。
- 证书不受信任时，Claude 报 API 错误后退出，不会绕过代理直连。
- WebFetch 的域名预检（向 `api.anthropic.com` 询问域名是否可以抓取）和抓取本身都经过代理；抓回的页面交给模型摘要时走 API。
- 打开非必要流量（tele 不设 `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`）时，Claude 的全部 TCP 连接都指向代理，它自己不做任何 DNS 查询。

## scratch 文件

Claude 在本地打开、远端命令也要访问的文件：

| 文件 | 谁写 | 谁读 | 处理 |
|---|---|---|---|
| cwd 文件（`pwd -P >| …`，位于 `CLAUDE_CODE_TMPDIR`） | 远端命令 | 本地 Claude | **回传** |
| shell 快照（`<config>/shell-snapshots/*.sh`） | 远端（生成脚本） | 远端命令 source；本地 Claude 检查它是否存在 | 远端保留一份；回传一份本地副本，用于通过存在性检查 |
| `CLAUDE_ENV_FILE`（`<config>/session-env/<Claude 会话 id>/sessionstart-hook-<n>.sh`，由 SessionStart hook 写入） | 远端 hook | 本地 Claude | 回传。hook 运行时本地只有它所在的目录，文件本身还不存在，所以上传要在远端建出这个空目录，否则 hook 的 `>> "$CLAUDE_ENV_FILE"` 在远端因目录不存在而失败 |
| `tasks/` 目录中的文件（`CLAUDE_CODE_TMPDIR/claude-<uid>/<cwd 编码>/<Claude 会话 id>/tasks/`） | 远端命令 | 本地 Claude | 回传；目录位于 `CLAUDE_CODE_TMPDIR` 之下 |
| 后台任务的输出文件 | Claude 在**本地** `open(path, "w")`，把 fd 作为子进程的 stdout | 本地 Claude | 远端输出经 shim 写入这个本地 fd；它如果位于 scratch 目录中，写入期间不参与同步（见 [scratch 路径改写与回传](exec.md#scratch-路径改写与回传)） |

`<config>` 是 Claude 的配置目录，即 `~/.claude`。机制见 [scratch 路径改写与回传](exec.md#scratch-路径改写与回传)。

**已知限制**：如果用户的命令显式引用后台任务的输出文件（例如 `tail <输出文件路径>`），远端看不到这个文件。Claude 通常用 Read 或 TaskOutput 读取它，影响很小。

## ~/.claude.json

Claude 写全局配置时，先在 `$HOME` 中创建 `.claude.json.tmp.<pid>.<随机>`，写完后 rename 覆盖 `.claude.json`；每次写之前还用 `mkdir` 在 `$HOME` 中建立 `.claude.json.lock` 作为锁。它在 `$HOME` 中直接创建的名字都以 `.claude.json` 开头（配置目录 `.claude` 除外），备份写在 `<config>/backups/`。

所以 `.claude.json` 不能用 bind 挂载单个文件的方式放进远端视图：rename 覆盖挂载点会失败（`EBUSY`），锁目录和临时文件也不能落到远端。远端 `HOME` 中以 `.claude.json` 开头的名字都必须落到本地 `HOME` 的同名条目上，这由 telefs 完成（见[组成](telefs.md#组成)中的「本地名字」）。

## 其它内置行为

| 行为 | 处理 |
|---|---|
| Read、Write、Edit、NotebookEdit，读取图片和 PDF，`@` 引用 | 进程内的文件系统调用 → telefs |
| 启动时加载的 `CLAUDE.md`、`.claude/*`、`.mcp.json` | 项目级的来自远端（telefs）。全局的 `~/.claude/*`、`/etc/claude-code/*` 在本地集合中，来自本地 |
| 设置文件的热加载（Claude 通过文件监视发现设置文件的变化） | 内核只为经过本地 VFS 的操作产生 inotify 事件，telefs 的缓存失效不会产生。所以远端命令对设置文件的改动不会触发热加载，经 Claude 自己的 Write/Edit 做的改动仍然会。影响很小 |
| WebFetch、WebSearch | 在本地或 Anthropic 侧执行，出站 IP 是本地的 |
| 超时与中断：先 SIGTERM，再 SIGKILL（tree-kill，会调用 `ps`） | shim 转发可捕获的信号。SIGKILL 无法捕获，由会话主进程发现 shim 的连接关闭后结束远端进程组（见 [shim 与会话主进程](exec.md#shim-与会话主进程)）。`ps` 走本地 exec 代理，因为它必须看到本地进程 |
| 系统提示词中的操作系统和平台 | Claude 在进程内取得内核版本，不启动 `uname`，所以内置的环境信息（`OS Version: Linux <版本>`）在 tele 下是**本地**的内核。这是已知限制：tele 不拦截系统调用，UTS 命名空间也只隔离主机名，不隔离内核版本。目标主机的信息由[附加系统提示词](#附加系统提示词)给出 |
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

验收标准：Claude 发给 API 的请求中，系统提示词包含目标主机的主机名和发行版。唯一的本地信息是内置环境信息中的内核版本（见[其它内置行为](#其它内置行为)）。兼容性测试在模拟 API 一侧检查这一点。

## 验证方法

- **兼容性测试**：用模拟的 Anthropic API 返回事先编排好的 tool_use，驱动真实的 `claude -p`，逐条检查本文的契约。测试在 `internal/claudecompat`，由 `TELE_TEST_CLAUDE=<claude 路径>` 启用；测试注释按小节标题指向本文，契约和测试要一起修改。其中观察系统调用的测试（Claude 自己启动了哪些程序、网络连接指向哪里）需要 `strace`，没有时跳过。`internal/launcher` 中还有用同一个变量启用的端到端测试：不切换视图，让真实的 Claude 经 shim、relay 和 tele server 走完整条 exec 链路，以及经 tele 自己的 CONNECT 代理访问 API。
- **每个新的 Claude Code 版本**在加入已验证列表之前都要重新验证：运行兼容性测试（最后一条检查被测版本已列入 `claudever.Verified`）；用 `strace -f` 对切换视图之后的文件访问和 exec 做差异比对（切换视图之前的 exec 和网络连接已由兼容性测试覆盖）。新出现的 dlopen、运行时文件，以及按名字或按绝对路径启动的程序，都要在预加载列表、本地集合或 shim 列表中处理。telefs 在 debug 日志中记录 Claude 进程对疑似运行时文件（`*.so*`、`/etc/ssl` 下的路径）的访问，用来排查这类回归。
- **逆向**：Claude Code 是 bun 编译的单文件二进制，内嵌压缩过的 JS。在二进制中搜索 `CLAUDE_CODE_SHELL_PREFIX`、`USE_BUILTIN_RIPGREP` 等字符串，就能找到相关实现。
- **文件访问**：用 `strace -f -e trace=%file,execve claude -p …` 观察 Claude 在启动、加载配置、发起一次 API 请求期间访问的路径。

## 待验证的行为

以下 Claude Code 和 bun 的行为决定了「先本地加载、再切换视图」能否成立，实现前必须逐条验证。验证通过后把结论作为契约写入上文、由兼容性测试守护，然后删除对应条目。

| 假设 | 不成立时的退路 |
|---|---|
| 在 `main` 之前，bun 仍然是单线程的（`setns(CLONE_NEWNS)` 要求进程不与其它线程共享文件系统信息） | 回到「bind 挂载动态库和 DNS 文件」的做法，本地例外变多 |
| 切换之后，Claude 不再 dlopen 或打开其它本地运行时文件 | 在切换前预先加载；实在不行，加入本地集合 |
| 本地 uid 在远端的 `/etc/passwd` 中可能不存在，但 `os.userInfo()` 等调用不受影响，或者 `USER`、`HOME` 足以兜底。另外，glibc 的 NSS 会按远端的 `nsswitch.conf` dlopen 远端的 `libnss_*.so`，远端 glibc 版本不同时可能崩溃 | 切换前预先加载本地的 NSS 模块，或在远端视图中合成 passwd 条目 |
| OAuth 令牌的刷新也遵循代理（其它出站连接见[代理与 CA](#代理与-ca)）。不走代理的连接会在远端视图中做 DNS 解析，从而失败 | 把本地的 DNS 配置文件加入本地集合 |
| bun 信任 `<sess>/ca-bundle.pem`（见[代理与 CA](#代理与-ca)）之后，不再依赖系统证书目录；切换到远端视图后，`/etc/ssl` 是远端的 | 把本地证书目录 bind 到 `/etc/ssl` 等路径，多一个本地例外 |
| 兼容性测试覆盖的功能中，Claude 自己按绝对路径启动的只有 `/bin/sh`，其余都按 PATH 查找。没有覆盖的功能（打开浏览器、剪贴板、通知等）启动的程序同样可以逐个列出并处理（见 [shim](exec.md#shim)） | 无 |
