# tele

tele 让 Claude Code 在本地运行，而 Bash、hooks、MCP server 和整个文件系统都在一台远端主机上。Go 实现，只支持 Linux，许可证 AGPL-3.0。

## 常用命令

```bash
make build        # 构建 bin/tele（先编译 teleswitch 预加载库，再嵌入）
make test         # 单元测试（-race），不需要特权
make lint         # golangci-lint，必须零告警
make check        # 提交前检查：格式、vet、lint、test
make vuln         # govulncheck
make dist         # 发布文件：各架构的归档、附带依赖的源码、SHA256SUMS（需要 aarch64-linux-gnu-gcc）
make test-priv    # 特权集成测试（userns、FUSE）：设置 TELE_TEST_REQUIRE_PRIV=1，环境不具备能力时失败而不是跳过；以 root 和普通用户各运行一遍
go test ./internal/<pkg> -run TestName   # 运行单个测试
```

`TELE_TEST_CLAUDE=<claude 路径>` 启用 Claude 兼容性测试和端到端测试（见 [docs/claude-code.md](docs/claude-code.md#验证方法)），部分测试还需要 `strace`。新的 Claude Code 版本通过全部兼容性测试后，才能加入 `claudever.Verified`。

## 文档

写代码前先读 [docs/coding-standards.md](docs/coding-standards.md)。

| 文档 | 内容 |
|---|---|
| [architecture.md](docs/architecture.md) | 目标、非目标与关键设计决策 |
| [claude-code.md](docs/claude-code.md) | tele 依赖的 Claude Code 行为（隐式契约）及其验证方法 |
| [filesystem.md](docs/filesystem.md) | Claude 进程看到的文件系统视图及其构建方式 |
| [telefs.md](docs/telefs.md) | 远端文件系统的 FUSE 实现 |
| [exec.md](docs/exec.md) | 命令的远程执行与 shim |
| [transport.md](docs/transport.md) | 传输与可恢复会话层 |
| [cli.md](docs/cli.md) | 命令行、启动流程、安装与预检 |
| [security.md](docs/security.md) | 信任边界与安全约束 |

## 容易出错的地方

- **Go 运行时是多线程的**：`unshare`、`setns`、`capset` 不能在普通 goroutine 中调用，要通过重新 exec 自己完成（见[系统调用、命名空间与进程](docs/coding-standards.md#系统调用命名空间与进程)）。
- **shim 的 stdio 属于被代理的命令**：绝不向其中写诊断信息，否则会破坏命令输出和 MCP 的 JSON-RPC 流。
- **视图切换必须失败关闭**：Claude 进程在本地视图中继续运行，会把本地文件当作远端文件修改。
- **errno 保真**：远端的 errno 原样传回本地，不折叠成 `EIO`。唯一的例外是 telefs 交给内核的 errno：内核只接受 1 到 511，超出范围的 FUSE 应答会让调用方永远挂起（见[组成](docs/telefs.md#组成)）。
- **抽象 unix socket 没有权限保护**：shim 和会话主进程都要用 `SO_PEERCRED` 校验对方的 uid，会话主进程还要校验会话 token（见[本地的会话主进程](docs/security.md#本地的会话主进程)）。
- **不要对本地集合的挂载点及其祖先发送 FUSE entry 失效**：可能卸下 bind 挂载（见[已知陷阱](docs/filesystem.md#已知陷阱)）。
- **Claude Code 的行为大多没有文档**：兼容性测试失败时，先确认并更新 `docs/claude-code.md`，再改代码。

## 约定

- 文档用中文；代码、注释、标识符和提交信息用英文。
- 文档之间、代码注释到文档都按小节标题引用，不写小节编号。
- 提交前运行 `make check`。改变行为的代码，与相应的文档更新放在同一个提交里。

## 文档规范

只记录从代码中看不出来的、长期有效的知识：

- 设计理由与取舍；
- 与外部系统的隐式契约；
- 不明显的失效模式和运维陷阱；
- 硬性的业务或合规约束。

不要写：

- 变更日志、任务清单、进度报告、实现过程的叙述；
- 方案对比或决策过程的历史；
- 容易过时的事实，例如版本号、路径清单、指标、状态快照；
- 更适合用代码或代码旁注释表达的细节。

待办工作只记在根目录的 [ROADMAP.md](ROADMAP.md)，完成即删除；`docs/` 中不写。

易变的信息引用它的权威来源，不要复制当前值。决策记录只在同时满足两个条件时保留：相关任务尚未完成，并且这个决策对今后的维护至关重要；否则删除。

代码使文档失效时，立即更新或删除受影响的内容，绝不把过时内容保留为「历史说明」。
