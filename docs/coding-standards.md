# 代码规范

适用于本仓库的全部 Go 代码、C 代码、测试和构建脚本。本文没有覆盖的地方，按 [Effective Go](https://go.dev/doc/effective_go)、[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) 和 [Google Go Style Guide](https://google.github.io/styleguide/go/) 执行。

## 语言与工具链

- **Go**：版本以 `go.mod` 的 `go` 指令为准（下限由 shadowsocks-go 决定），`GOTOOLCHAIN=auto` 会自动拉取对应工具链。
- **只支持 Linux**：不写 `//go:build linux`；只有同一个包需要按 CPU 架构或内核特性区分实现时，才使用构建约束。
- **不使用 cgo**：发布构建是 `CGO_ENABLED=0`、`-trimpath` 的静态二进制。唯一的 C 代码是 teleswitch 预加载库（见 [C 代码](#c-代码teleswitch)），它单独编译，再嵌入 Go 二进制。
- **构建入口是 `Makefile`**：`make build`、`make test`、`make test-priv`、`make lint`，`make check` 汇总所有提交前检查。CI 和本地执行同样的目标。

## 目录与包

- `cmd/tele` 是唯一的 `main` 包，只负责 multi-call 分派（按 `argv[0]` 或子命令），不包含业务逻辑。
- 所有实现放在 `internal/` 下。本项目不对外提供 Go API，所以没有 `pkg/`。
- 一个包只负责一件事。包名用简短的小写名词，不用 `util`、`common`、`helpers`、`misc` 这类名字。
- 每个包都要有包注释，说明它负责什么，以及从代码里看不出来的约束。
- **依赖方向**：`cmd` → 编排层（启动器、服务端）→ 功能层（exec、telefs、shim 等）→ 协议层（`internal/proto`）→ 标准库和第三方库。禁止反向依赖和循环依赖。
- 本地侧和远端侧只通过协议层共享类型：远端的服务代码不引用本地侧的包，反过来也一样。唯一的例外是会话主进程为 telefs 的本地名字在进程内运行 fssvc（见[组成](telefs.md#组成)），由编排层直接引用。
- 测试辅助代码放在 `internal/testutil/` 下，只能被 `_test.go` 引用。

## 格式、命名与注释

- 用 `gofmt` 和 `goimports` 格式化，本模块的 import 单独分为一组。
- 以仓库里的 `.golangci.yml` 为准，`make lint` 必须零告警。要屏蔽某条告警，只能写 `//nolint:<linter> // <原因>`，并且只作用于那一行。
- 命名遵循 Go 惯例：缩写词大小写保持一致（`ID`、`FUSE`、`PSK`、`sessionID`），同一类型的方法接收者名在整个包内保持一致。
- **注释用英文**，写「为什么」和约束，不复述代码在做什么。导出的标识符都要有文档注释，并以标识符名开头。
- **与外部系统的隐式契约**（Claude Code 的未公开行为、内核语义、FUSE 协议细节）必须在实现它的代码旁注明，并按标题指向 `docs/` 中的对应小节（例如 `docs/exec.md "exec 屏障"`），不写小节编号。这些地方是版本升级时最容易出问题的点。
- 函数超过约 60 行或嵌套超过 3 层时应该拆分。文件长度不设硬性上限。

## 错误处理

- 所有 error 都必须处理。确实要忽略时，写成 `_ = f()` 并注释原因。
- 包装错误用 `fmt.Errorf("open session dir: %w", err)`：小写开头，末尾不加句号，不以 `failed to`、`error` 开头，也不重复下层已经给出的信息。
- 判断错误用 `errors.Is` / `errors.As`。包级哨兵错误定义为 `var ErrXxx = errors.New("<pkg>: ...")`。
- **errno 必须保真**：telefs 和 exec 链路上，远端系统调用返回的 `unix.Errno` 要原样传回本地，telefs 交给内核的就是远端的 errno。只有本地确实无法知道原因时（例如会话租约已过期）才使用 `EIO`。
  - 唯一的例外是内核无法表示的 errno：内核拒绝错误码不在 1 到 511 之间的 FUSE 应答，而 go-fuse 忽略这次写失败，请求永远得不到应答，调用方一直挂起（SIGKILL 也无效），直到 FUSE 连接被中止。这类 errno 是远端文件系统泄漏出来的内核内部错误码，所以 telefs 把 `ENOTSUPP`（524，NFS 会泄漏它）换成 `EOPNOTSUPP`，其余换成 `EIO`，并记录警告。
- `panic` 只用于程序员错误（违反了不变量）。可以恢复的运行时错误一律返回 error。唯一允许 `recover` 的位置是服务端每个会话的顶层 goroutine：记录堆栈，然后关闭**整个**会话，不能只丢掉出错的那个请求继续运行。
- 面向用户的错误信息要说明三件事：发生了什么、可能的原因、下一步怎么做（例如提示运行 `tele doctor <别名>`）。

## 并发

- 每个 goroutine 都要有明确的所有者和退出条件，由启动它的一方负责等待它退出（`errgroup`、`sync.WaitGroup`），不允许启动后不管。
- 取消通过 `context.Context` 传递，并作为函数的第一个参数。不要把 ctx 存进结构体；长生命周期对象确实需要时，必须注释说明。
- 互斥锁紧挨着它保护的字段声明，并注释保护范围。持锁期间不做网络或磁盘 I/O，也不调用外部传入的回调。
- channel 由发送方关闭，并注释谁负责关闭。
- 每一个网络等待都必须有超时或者可以被取消。「无限等待」只能是有意的设计（例如 telefs 在断线期间阻塞），并且要注释说明。
- 测试一律用 `-race` 运行。会启动 goroutine 的包在 `TestMain` 中用 `goleak` 检查泄漏。

## 系统调用、命名空间与进程

- 使用 `golang.org/x/sys/unix`。`syscall` 包只在 `os/exec` 要求时使用（例如 `syscall.SysProcAttr`）。
- **Go 运行时是多线程的**。`unshare`、`setns`、`capset`、`prctl` 这类只作用于当前线程的调用，不能在普通 goroutine 里执行：
  - 要改变整个进程的命名空间，就重新 exec 自己，并通过 `SysProcAttr`（`Cloneflags`、`Unshareflags`、`AmbientCaps`）完成；
  - 确实必须在当前线程上执行时，先调用 `runtime.LockOSThread` 并且不再解锁，让这个线程随 goroutine 结束而销毁。
- fd 默认带 `O_CLOEXEC`（Go 的默认行为）。要交给子进程的 fd 只能通过 `ExtraFiles` 显式传递，fd 编号的约定写在常量和注释里。
- `x/sys/unix` 的原始调用不会自动重试 `EINTR`，调用方需要自己处理。
- 子进程放在独立的进程组中，必须等待并回收。退出状态要原样传播：被信号杀死和以某个退出码退出是两种不同的结果，shim 被信号杀死时，要以同一个信号结束自己。
- 创建文件和目录时显式指定权限，不依赖 umask：密钥文件 0600，私有目录 0700。

## 协议

- 跨进程、跨主机的消息类型全部定义在 `internal/proto`，用 CBOR 编码（整数键），外层是长度前缀的帧。每种流都有最大帧长，**先检查长度，再解码**。
- 建立会话时交换协议版本。两端版本不兼容时要明确报错，并提示应该升级哪一端，不能让它表现为解码失败。
- 字段只能新增，不能改变已有字段的含义。删除的字段编号保留，不再复用。
- 对端发来的所有数据都视为不可信：校验长度、枚举取值和路径（路径必须是绝对路径，不能包含 NUL）。

## 安全

安全约束以 [security.md](security.md) 为准，编码时落实为：

- 密钥（PSK、配对 token、会话 token、代理密码）比较时用 `crypto/subtle.ConstantTimeCompare`，不写入日志和错误信息，存储文件的权限为 0600。
- 远端返回的路径不能直接用于本地文件操作，校验要求见[远端返回的数据](security.md#远端返回的数据)。
- 视图切换失败时必须失败关闭（见 [teleswitch](filesystem.md#teleswitch)）。

## C 代码（teleswitch）

- 只用于「必须在 `main` 之前、进程还是单线程时执行」的逻辑，保持最小。它不拦截任何函数（见[设计决策](architecture.md#设计决策)）。
- **不链接 libc**（`-nostdlib`），不产生 `DT_NEEDED`，只使用内联的原始系统调用，不分配内存。这样它不依赖目标进程的 libc 实现和版本（glibc 或 musl）。
- 编译选项以 `Makefile` 为准，至少包含 `-Wall -Wextra -Werror`。每个系统调用的失败都要处理：失败时向诊断 fd 写一行诊断信息，然后以专用退出码 `_exit`，保证视图切换失败时失败关闭。诊断 fd、退出码和环境变量名定义为 Go 与 C 两侧共享的常量，并在常量旁注释。
- 由 `make` 按目标架构编译后放入嵌入目录，产物不提交到仓库；嵌入目录中提交一个占位文件，使没有运行 `make` 时 `go build`、`go test ./...` 仍能编译。二进制中缺少这个库时，运行期要明确报错，不能静默跳过。

## 日志与输出

- 诊断信息用 `log/slog` 输出结构化日志。
- **shim 的 stdin、stdout、stderr 属于被代理的命令**。shim 和会话主进程绝不能往里面写诊断信息，否则会破坏命令输出和 MCP 的 JSON-RPC 流。诊断信息写入会话日志；遇到致命错误时，只在 stderr 写一行 `tele: <原因>`，然后以约定的退出码退出。
- 日志中不记录密钥和文件内容。命令行和路径只在 debug 级别记录。

## 测试

- 测试和代码放在同一个包的 `_test.go` 中，优先写表驱动测试，只用标准库 `testing`，不引入断言库。
- 不用 `sleep` 做同步。需要等待时用 channel，或者带超时的轮询辅助函数。
- **分层**：
  1. **单元测试**：不需要特权和网络，`go test ./...` 必须能以普通用户身份在任何 Linux 上通过。
  2. **特权集成测试**（userns、FUSE、`pivot_root`）：先检测环境能力，不满足时 `t.Skip` 并写明原因。设置 `TELE_TEST_REQUIRE_PRIV=1`（`make test-priv`）时改为直接失败，避免 CI 中的测试被静默跳过。
  3. **Claude 兼容性测试**：用模拟的 Anthropic API 按脚本驱动真实的 `claude -p`，由 `TELE_TEST_CLAUDE=<claude 路径>` 启用。它用来验证 [claude-code.md](claude-code.md) 中的每一条契约。辅助代码在 `internal/testutil/claudetest`（模拟 API、CONNECT 代理、strace 包装）。这一层是可选启用的：没有设置变量时跳过，`TELE_TEST_REQUIRE_PRIV` 不影响它。
  4. **故障注入**：会话层在 `net.Conn` 这一层注入断线、延迟、半开连接和乱序重连；端到端测试用 `tc netem`、toxiproxy 和网络命名空间切换，模拟丢包、断流和 IP 变化。
- 新功能和 bug 修复都必须带测试。修 bug 时，先写一个能复现问题的失败测试。
- 处理外部输入的函数（协议解码、路径改写、命令行解析）要有 fuzz 测试。
- POSIX 语义的回归测试套件在编写 telefs 测试时选定，以测试代码为准。

## 依赖

新增第三方依赖需要同时满足：

- 许可证与 AGPL-3.0 兼容（BSD、MIT、Apache-2.0、MPL-2.0、GPL-3.0、AGPL-3.0）；
- 仍在积极维护；
- 不能用少量代码替代。

引入理由写在提交说明里。版本由 `go.mod` 固定，发布前运行 `govulncheck`。

## 提交

- 提交信息使用 Conventional Commits 格式：`<type>(<scope>): <summary>`，type 取 `feat`、`fix`、`refactor`、`test`、`docs`、`build`、`chore` 之一。summary 用英文祈使句，不超过 72 个字符。
- 每个提交都必须能构建，并通过 `make check`。
- 改变了行为的代码，和相应的 `docs/` 更新放在同一个提交里。
