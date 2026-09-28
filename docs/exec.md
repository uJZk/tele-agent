# 远程执行

Claude 启动的命令由 shim 接住，交给会话主进程，再经会话通道在远端执行。本文说明 shim 的种类、shim 与会话主进程之间的约定，以及远端执行的语义。Claude 侧的注入点见 [claude-code.md](claude-code.md)。

## 1. shim

shim 都是指向 `tele` 的符号链接，位于 `<sess>/bin/`，按 `argv[0]` 分派。

| shim | 谁调用 | 行为 |
|---|---|---|
| `bash` | Bash 工具（`CLAUDE_CODE_SHELL`），以及生成 shell 快照 | 在远端运行 `bash`，参数原样转发。`-c` 的脚本如果是 `'<sess>/bin/tele-exec' '<命令串>'` 的包装形式，先去掉这一层 |
| `tele-exec` | shell 形式的 hooks、stdio MCP server（`CLAUDE_CODE_SHELL_PREFIX`） | 唯一的参数是一条 shell 字符串，在远端用 `sh -c` 执行，双向转发 stdio |
| `sh` | `/bin/sh`（`shell: true` 的 spawn 固定使用它，hooks 就是这样启动的） | `-c` 的脚本如果是 `'<sess>/bin/tele-exec' '<命令>'` 的形式，直接按 tele-exec 处理；否则在远端运行 `sh`，参数原样转发。必须识别这种形式，因为远端并不存在 `<sess>/bin/tele-exec` |
| `rg`、`git`、`uname` | Grep、Glob、Claude 内部的 git 调用、Claude 启动时获取平台信息 | 在远端运行同名程序，参数原样转发 |
| 本地 exec 代理（例如 `ps`） | 必须看到本地进程或本地桌面的调用：tree-kill 用 `ps` 查找子进程，以及打开浏览器、访问剪贴板一类的程序 | 由会话主进程在**本地视图**中执行真实程序 |

- `PATH` 中只有 `<sess>/bin`，Claude 按名字启动、却不在上表中的程序会得到 ENOENT。需要哪些本地 exec 代理，以 `strace` 观察到的 Claude 实际调用为准。
- **陷阱**：Claude 如果按**绝对路径**启动程序（例如 `/usr/bin/xdg-open`），在远端视图中它会读到远端的可执行文件，却在本地内核上运行。这类调用同样要用 `strace` 找出来，逐个处理。
- 远端按 argv 执行程序（`rg`、`git`、`uname`）时，使用目标用户**登录 shell 的 PATH**。这个 PATH 在建立会话时获取一次，因为以服务方式运行的 tele server 自己的 PATH 通常很短。
- 远端缺少 `bash`、`rg` 或 `git` 时，对应的功能会失败。`tele server install` 和 `tele doctor` 检查并给出提示。

## 2. shim 与会话主进程

- shim 连接抽象 unix socket（`TELE_SOCK`），发送请求：种类（远端执行或本地执行）、argv、cwd、环境变量和会话 token，同时通过 `SCM_RIGHTS` 把自己的 fd 0、1、2 交给会话主进程。
- 此后由会话主进程直接读写这些 fd，shim 本身不转发任何数据，只负责转发信号和等待退出状态。这样做的原因是：
  - 转发数据不需要多一次拷贝；
  - 后台任务的输出文件是 Claude 在本地打开、作为子进程 stdout 传下来的 fd，会话主进程可以直接写入；
  - 会话主进程可以对这些 fd 做 `isatty` 判断，决定远端是否分配 pty。
- 会话主进程用 `SO_PEERCRED` 校验对端 uid，并校验会话 token（[security.md](security.md)）。
- **退出状态原样传播**：远端以退出码 N 结束时，shim 以 N 退出；远端被信号 S 杀死时，shim 恢复 S 的默认处理后向自己发送 S，让 Claude 看到同样的结果。
- 会话主进程在 shim 的 socket 上读到 EOF、却还没有送出退出状态时，说明 shim 被 SIGKILL 了（Claude 的超时或中断），这时通过会话通知远端结束整个进程组，然后关闭手中的 fd。这种情况必须与网络断线区分开：网络断线时远端进程继续运行（[transport.md](transport.md) 第 3 节）。

## 3. 环境变量

只转发**增量**：以 tele 交给 Claude 的环境为基线，转发 Claude 新增或修改过的变量，再加上一份白名单（`TERM`、`COLORTERM`、`LANG`、`LC_*` 等）。

以下变量**绝不转发**，它们只对本地的 Claude 进程有意义，或者指向本地资源：

- `PATH`、`HOME`：远端使用自己的值；
- `LD_PRELOAD` 和所有 `TELE_*`；
- `HTTPS_PROXY`、`HTTP_PROXY`、`NO_PROXY`、`ALL_PROXY`（大小写两种形式）：指向本地的 CONNECT 代理；
- `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`：指向本地的 CA bundle；
- `CLAUDE_CODE_SHELL`、`CLAUDE_CODE_SHELL_PREFIX`、`USE_BUILTIN_RIPGREP`：指向本地的 shim；
- `SSH_AUTH_SOCK`、`DISPLAY`、`WAYLAND_DISPLAY`、`XDG_RUNTIME_DIR`、`DBUS_SESSION_BUS_ADDRESS`：本地桌面和本地会话的资源。

转发的变量值同样要做 scratch 路径改写（第 5 节）。

## 4. 进程与信号

- 远端的每条命令放在独立的进程组中运行。
- shim 捕获可以捕获的信号（SIGINT、SIGTERM、SIGHUP、SIGQUIT、SIGUSR1、SIGUSR2，分配了 pty 时还有 SIGWINCH），转发给远端的进程组。
- stdout 和 stderr 分开传输，不合并。
- 断线期间远端进程继续运行，输出缓存在服务端（有界，超出部分写入磁盘），shim 阻塞等待。超过会话租约后，远端先对进程组发送 SIGTERM，再发送 SIGKILL，shim 在 stderr 写一行 `tele: …` 并以错误状态退出。

## 5. scratch 路径改写与回传

Claude 在本地打开、远端命令也要访问的文件，列在 [claude-code.md](claude-code.md) 第 4 节。

- scratch 前缀有三个：`CLAUDE_CODE_TMPDIR`、`<config>/shell-snapshots`、`<config>/session-env`（`<config>` 是 Claude 的配置目录，即 `~/.claude`）。前缀必须足够具体，不会误匹配用户命令中的普通路径。
- **执行前**：命令串、argv 和转发的环境变量中出现的 scratch 前缀，改写为远端的会话目录 `~/.cache/tele/s/<sid>/…`。本地 scratch 文件自上次同步后有变化的，先上传。
- **执行后**：远端把会话目录中本次变化过的小文件（每个不超过 1 MB）随退出状态一起回传，会话主进程写回本地。只允许写入本地的 scratch 目录：先规范化路径、再校验前缀，并拒绝经由符号链接逃出该目录。

## 6. exec 屏障

命令结束时，退出状态中带有服务端变更推送的序号 W：服务端回收子进程后，先把 inotify 队列中已有的事件全部读出并编号，再发出退出状态。会话主进程等 telefs 应用完序号不超过 W 的所有失效通知后，才把退出状态交给 shim。inotify 事件是在文件系统操作中同步入队的，所以命令造成的变更一定在 W 之内。

这保证了「Bash 修改文件 → 紧接着 Read」一定读到新内容（[telefs.md](telefs.md) 第 3 节）。

## 7. 端口转发

URL 指向 `localhost` 的 HTTP 或 SSE 形式的 MCP server 实际运行在远端。会话主进程按 `.mcp.json` 中的端口，建立本地 `127.0.0.1:N` 到远端 `127.0.0.1:N` 的 TCP 转发，转发流同样走会话通道。

不能把所有发往 localhost 的连接都转到远端：Claude 与 IDE 插件等本地程序之间的 localhost 连接必须留在本地。
