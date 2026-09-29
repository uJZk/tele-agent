# 远程执行

Claude 启动的命令由 shim 接住，交给会话主进程，再经会话通道在远端执行。本文说明 shim 的种类、shim 与会话主进程之间的约定，以及远端执行的语义。Claude 侧的注入点见 [claude-code.md](claude-code.md)。

## shim

shim 都是指向 `tele` 的符号链接，位于 `<sess>/bin/`，按 `argv[0]` 分派。

| shim | 谁调用 | 行为 |
|---|---|---|
| `bash` | Bash 工具（`CLAUDE_CODE_SHELL`），以及生成 shell 快照 | 在远端运行 `bash`，参数原样转发。`-c` 的脚本如果恰好是 `<sess>/bin/tele-exec` 加一个参数这两个 shell 词，先去掉这一层包装 |
| `tele-exec` | shell 形式的 hooks、stdio MCP server（`CLAUDE_CODE_SHELL_PREFIX`） | 唯一的参数是一条 shell 字符串，在远端用 `sh -c` 执行，双向转发 stdio |
| `sh` | `/bin/sh`（`shell: true` 的 spawn 固定使用它，hooks 就是这样启动的） | `-c` 的脚本如果恰好是 `<sess>/bin/tele-exec` 加一个参数，直接按 tele-exec 处理；否则在远端运行 `sh`，参数原样转发。必须识别这种形式，因为远端并不存在 `<sess>/bin/tele-exec` |
| `rg`、`git`、`uname` | Grep、Glob，Claude 内部的 git 和 rg 调用；`uname` 给按名字调用它的程序（Claude 自己不调用，见[其它内置行为](claude-code.md#其它内置行为)） | 在远端运行同名程序，参数原样转发 |
| 本地 exec 代理（例如 `ps`） | 必须看到本地进程或本地桌面的调用：tree-kill 用 `ps` 查找子进程，以及打开浏览器、访问剪贴板一类的程序 | 由会话主进程在**本地视图**中执行真实程序 |

- `PATH` 中只有 `<sess>/bin`，Claude 按名字启动、却不在上表中的程序会得到 ENOENT。需要哪些本地 exec 代理，以 `strace` 观察到的 Claude 实际调用为准。
- **陷阱**：Claude 如果按**绝对路径**启动程序（例如 `/usr/bin/xdg-open`），在远端视图中它会读到远端的可执行文件，却在本地内核上运行。这类调用同样要用 `strace` 找出来，逐个处理。
- 远端按 argv 执行程序（`rg`、`git`、`uname`）时，使用目标用户**登录 shell 的 PATH**。这个 PATH 在建立会话时获取一次，因为以服务方式运行的 tele server 自己的 PATH 通常很短。
- 远端缺少 `bash`、`rg` 或 `git` 时，对应的功能会失败。`tele server install` 和 `tele doctor` 检查并给出提示。
- 基础设施故障（会话主进程不可达、会话已断开等）时，shim 在 stderr 写一行 `tele: <原因>`，以退出码 255 退出。

## shim 与会话主进程

- shim 从环境变量 `TELE_SESSION` 得到会话目录，由它得出抽象 socket 的名字（`@tele-<sid>`），并读取目录中的会话 token 文件。
- shim 连接 socket 后，先发送一个字节，并通过它的 `SCM_RIGHTS` 把自己的 fd 0、1、2 交给会话主进程；然后发送请求：argv、cwd、完整的环境变量和 token。分类（哪种 shim、是否去掉包装、本地还是远端）由会话主进程完成，shim 本身保持最简单。
- 此后由会话主进程直接读写这些 fd，shim 不转发任何数据，只负责转发信号和等待退出状态。这样做的原因是：
  - 转发数据不需要多一次拷贝；
  - 后台任务的输出文件是 Claude 在本地打开、作为子进程 stdout 传下来的 fd，会话主进程可以直接写入；
  - 会话主进程可以对这些 fd 做 `isatty` 判断，决定远端是否分配 pty。
- 会话主进程不改变收到的 fd 的标志（例如 `O_NONBLOCK`）：这些 fd 与 Claude 共享打开文件描述，stdin 甚至可能是用户的终端。
- **双向校验**（见[本地的会话主进程](security.md#本地的会话主进程)）：
  - 会话主进程用 `SO_PEERCRED` 校验 shim 的 uid，并校验会话 token；
  - shim 连接后、发送 token 和 fd 之前，同样用 `SO_PEERCRED` 确认监听方的 uid 是自己的 uid。
- **拒绝的报告方式**：
  - 对端 uid 不对时，会话主进程直接关闭连接，不说明原因；
  - uid 正确、但请求被拒绝时（token 不对、请求不合法、握手超时、会话正在结束），会话主进程回送退出码 255 和原因，shim 把原因写到 stderr；
  - 连接关闭、却没有收到退出状态时，shim 提示去看 `<TELE_SESSION>/log`。所以会话目录中的 `log` 是一个隐式契约：会话主进程的日志必须写在这里（文件名见 `shimsrv.LogFile`）。
- **请求的大小**：argv 加环境变量最多可达 6 MiB（`RLIMIT_STACK` 很大时的 `ARG_MAX`；默认 8 MiB 栈时为 2 MiB），所以每一跳承载命令行的帧上限都必须容得下它。shim 这一跳的上限是 `proto.MaxShimRequest`；shim 在连接之前先编码请求，超出上限时直接报错，提示用文件传递大数据。到远端的 `ExecStart` 帧上限（`proto.MaxExecStart`）为 shim 请求的上限加上一次 scratch 上传的上限，所以命令行再大也不会挤占随它上传的 scratch 文件；之后的 exec 帧仍以 `proto.MaxDataFrame` 为上限。
- **退出状态原样传播**：远端以退出码 N 结束时，shim 以 N 退出；远端被信号 S 杀死时，shim 恢复 S 的默认处理后向自己发送 S，让 Claude 看到同样的结果。
- **输出可能晚于退出**：远端命令的主进程结束时，shim 就退出，这与本地命令的进程退出时机一致；如果后台子进程仍然持有输出管道，会话主进程继续转发输出，直到远端管道关闭，然后才关闭手中的 fd。所以 Claude 看到的管道关闭时机也与本地一致。
- 会话主进程在 shim 的 socket 上读到 EOF、却还没有送出退出状态时，说明 shim 被 SIGKILL 了（Claude 的超时或中断），这时通过会话通知远端结束整个进程组，然后关闭手中的 fd。这种情况必须与网络断线区分开：网络断线时远端进程继续运行（见[断线语义](transport.md#断线语义)）。

## 环境变量

只转发**增量**：以 tele 交给 Claude 的环境为基线，转发 Claude 新增或修改过的变量，再加上一份白名单（`TERM`、`COLORTERM`、`LANG`、`LC_*` 等）。

以下变量**绝不转发**，它们只对本地的 Claude 进程有意义，或者指向本地资源：

- `PATH`、`HOME`：远端使用自己的值；
- `LD_PRELOAD` 和所有 `TELE_*`；
- `HTTPS_PROXY`、`HTTP_PROXY`、`NO_PROXY`、`ALL_PROXY`（大小写两种形式）：指向本地的 CONNECT 代理；
- `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`：指向本地的 CA bundle；
- `CLAUDE_CODE_SHELL`、`CLAUDE_CODE_SHELL_PREFIX`、`USE_BUILTIN_RIPGREP`：指向本地的 shim；
- `SSH_AUTH_SOCK`、`DISPLAY`、`WAYLAND_DISPLAY`、`XDG_RUNTIME_DIR`、`DBUS_SESSION_BUS_ADDRESS`：本地桌面和本地会话的资源。

转发的变量值同样要做 scratch 路径改写。

## 进程与信号

- 远端的每条命令放在独立的进程组中运行。
- shim 捕获可以捕获的信号（SIGINT、SIGTERM、SIGHUP、SIGQUIT、SIGUSR1、SIGUSR2，分配了 pty 时还有 SIGWINCH），转发给远端的进程组。
- stdout 和 stderr 分开传输，不合并（分配了 pty 时只有一路输出）。
- **信号不是带外的，但不会被 stdin 堵住**：信号和 stdin 数据走同一条 exec 流，排在 stdin 数据之后。所以 stdin 在流窗口之外还有自己的窗口（`proto.ExecStdinWindow`）：服务端把数据交给命令之后，才用 `ExecStdinAck` 归还窗口（一段连续输入合并确认一次），会话主进程未确认的 stdin 不超过这个窗口。服务端手里命令还没读的 stdin 因此有上限，它的读帧循环从不等待命令读取输入，排在后面的信号、终端尺寸变化和流结束总能立即处理。超出窗口的客户端违反协议，服务端结束这条命令。
- **没被读走的输入留在本地**：窗口用完时，会话主进程停止读取本地的 stdin。命令关闭了自己的 stdin 之后，服务端丢弃后来的数据、不再确认，会话主进程于是不再读取。这些 fd 与 Claude 共享（见 [shim 与会话主进程](#shim-与会话主进程)），没被命令读走的输入要留给下一个读者，这与本地命令的行为一致。
- 断线期间远端进程继续运行，输出缓存在服务端（有界，超出部分写入磁盘），shim 阻塞等待。这份缓存由会话层负责：exec 服务自己只缓冲到流窗口为止，窗口和输出管道写满后，命令阻塞在写输出上，直到会话恢复。超过会话租约后，远端先对进程组发送 SIGTERM，宽限期过后再发送 SIGKILL，shim 在 stderr 写一行 `tele: …` 并以错误状态退出。
- **已知限制**：
  - shim 启动时就被忽略的 SIGINT、SIGHUP（`nohup`、非交互 shell 的后台作业）保持忽略，不转发；而远端命令并不继承这种忽略状态，shim 也无法把它传过去。
  - 工作目录按 `getcwd(2)` 得到的物理路径发送，而不是逻辑上的 `$PWD`。这与 Claude 自己用 `pwd -P` 跟踪 cwd 一致，但远端的 `pwd` 显示的是解析后的路径。

## scratch 路径改写与回传

Claude 在本地打开、远端命令也要访问的文件，列在 [scratch 文件](claude-code.md#scratch-文件)。

- scratch 前缀覆盖所有需要回传的文件所在的目录：`CLAUDE_CODE_TMPDIR`、`<config>/shell-snapshots`、`<config>/session-env`（`<config>` 即 `~/.claude`）。`CLAUDE_CODE_TMPDIR` 由 tele 设为 `<sess>/tmp`，路径中含随机的会话 id，不会误匹配用户的路径；另外两个前缀在 `~/.claude` 下，而 `~/.claude` 在远端视图中本来就由本地集合提供，所以改写它们与 Claude 看到的内容一致。
- 前缀只在后面紧跟 `/`、字符串结尾或不能出现在路径段中的字符时才改写，`<sess>/tmpx` 不会被当作 `<sess>/tmp`。
- **执行前**：命令串、argv、cwd 和转发的环境变量中出现的 scratch 前缀，改写为远端会话目录下的对应目录。本地 scratch 文件自上次同步后有变化的，随 `ExecStart` 上传；会话开始前就已存在的文件（例如以前留下的快照）不上传。
- **只上传本会话的文件**：`CLAUDE_CODE_TMPDIR` 属于本会话，其中的文件都参与同步。`shell-snapshots` 和 `session-env` 由本机所有 Claude 会话共享，其中可能有别的会话（甚至别的主机）的 SessionStart hook 写下的凭证，所以只上传远端回传过的文件，以及本会话的命令中出现过的条目（前缀之后的第一个路径段，例如 `session-env/<Claude 会话 id>`）。把共享目录中的变化全部上传，会把其它会话的文件泄露给这台主机，而远端是不可信的（见[信任边界](security.md#信任边界)）。
- **认领的目录即使为空也要建出来**：同步只传普通文件，但 SessionStart hook 运行时，`CLAUDE_ENV_FILE` 所在的 `session-env/<Claude 会话 id>/` 里还没有任何文件（见 [scratch 文件](claude-code.md#scratch-文件)）。所以本会话认领的条目在本地是目录时，每次上传都带上它，由远端建出这个目录。这样不需要记录哪些目录已经建过，被命令删掉的目录也会在下一条命令前重建；认领的条目很少，代价只是几个字节。认领发生在改写命令时，所以一条命令要先改写、再收集它的上传。
- **上传以服务端写入为准**：服务端在尝试启动命令之前写入上传的文件，并在退出状态中报告已经写入，上传以此为准完成同步。所以命令不存在、工作目录不存在等启动失败时，上传照样完成；只有服务端拒绝了整个请求（请求不合法）时什么都没写，这些文件下次重新上传。收不到退出状态时（流中断、shim 被杀），命令已经启动也说明文件已经写入。一次上传的大小按编码后的帧大小和条目数限制，放不下的留给下一条命令。
- **服务端尽力应用上传**：写不进去的文件记录日志后跳过，命令照常执行。否则一个持续的失败（例如文件和目录同名冲突、磁盘已满）会让之后的每条命令都失败。
- **读不了不等于删除**：本地和远端扫描时遇到无法读取的文件或目录，都不会把它们（及其下的内容）当作已删除来上传或回传。
- **正在写入的输出文件不参与同步**：命令的输出文件（例如位于 `CLAUDE_CODE_TMPDIR` 中的后台任务输出文件）在会话主进程写入期间既不上传，也不会被回传的文件覆盖或删除。否则每条命令都会上传一份写了一半的文件，而回传的副本 rename 覆盖它之后，输出会继续写进一个已经被删除的文件。
- **执行后**：远端把会话目录中本次变化过的文件随退出状态一起回传，会话主进程写回本地。只允许写入本地的 scratch 目录：先规范化路径、再校验前缀，并拒绝经由符号链接逃出该目录。
  - 服务端应用上传造成的变化（包括同时运行的其它命令的上传）不回传，否则本地会把自己的上传再写回一次，可能覆盖更新的本地文件。
  - 回传有单个文件、总量、编码大小和条目数的上限。变化的文件从小到大选取，因为 Claude 等待的 cwd 文件、快照、`CLAUDE_ENV_FILE` 和标记文件都很小，不能被写入或删除大量文件的命令挤掉；然后才是删除。放不下的变化记录日志后丢弃，之后也不会补传。
- 命令删除了某个 scratch 目录（例如 `rm -rf "$CLAUDE_CODE_TMPDIR"`，改写后指向远端的对应目录）时，服务端在下一次使用前重新创建它。

## exec 屏障

命令结束时，退出状态中带有服务端变更推送的序号 W：服务端回收子进程后，先把 inotify 队列中已有的事件全部读出并编号，再发出退出状态。会话主进程等 telefs 应用完序号不超过 W 的所有失效通知后，才把退出状态交给 shim。inotify 事件是在文件系统操作中同步入队的，所以命令造成的变更一定在 W 之内。

这保证了「Bash 修改文件 → 紧接着 Read」一定读到新内容（见[一致性](telefs.md#一致性)）。

## 端口转发

URL 指向 `localhost` 的 HTTP 或 SSE 形式的 MCP server 实际运行在远端。会话主进程按 `.mcp.json` 中的端口，建立本地 `127.0.0.1:N` 到远端 `127.0.0.1:N` 的 TCP 转发，转发流同样走会话通道。

不能把所有发往 localhost 的连接都转到远端：Claude 与 IDE 插件等本地程序之间的 localhost 连接必须留在本地。所以 Claude 的 `NO_PROXY` 包含回环地址，回环连接不经过 CONNECT 代理。
