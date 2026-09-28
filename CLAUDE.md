# tele

tele 让 Claude Code 在本地运行，而 Bash、hooks、MCP server 和整个文件系统都在一台远端主机上。Go 实现，只支持 Linux，许可证 AGPL-3.0。

## 常用命令

```bash
make build        # 构建 bin/tele（先编译 teleswitch 预加载库，再嵌入）
make test         # 单元测试（-race），不需要特权
make lint         # golangci-lint，必须零告警
make check        # 提交前检查：格式、vet、lint、test
make test-priv    # 特权集成测试（userns、FUSE），要求环境具备能力，否则失败
go test ./internal/<pkg> -run TestName   # 运行单个测试
```

`TELE_TEST_CLAUDE=<claude 路径>` 启用 Claude 兼容性测试（[docs/claude-code.md](docs/claude-code.md) 第 8 节）。

## 文档

写代码前先读 [docs/coding-standards.md](docs/coding-standards.md)。

| 文档 | 内容 |
|---|---|
| [architecture.md](docs/architecture.md) | 目标与非目标、设计决策及理由、进程与角色 |
| [claude-code.md](docs/claude-code.md) | 与 Claude Code 的隐式契约：注入点、scratch 文件、注入的环境、系统提示词 |
| [filesystem.md](docs/filesystem.md) | 远端视图、本地集合、命名空间构建、teleswitch、已知陷阱、待验证假设 |
| [telefs.md](docs/telefs.md) | FUSE 文件系统：协议、一致性、变更监视、断线恢复 |
| [exec.md](docs/exec.md) | shim、shim 与会话主进程的约定、环境变量、信号、scratch 改写、exec 屏障 |
| [transport.md](docs/transport.md) | SS2022、可恢复会话层、断线语义 |
| [cli.md](docs/cli.md) | 命令行、启动流程、安装配对、预检与修复策略 |
| [security.md](docs/security.md) | 信任边界与安全约束 |

## 容易出错的地方

- **Go 运行时是多线程的**：`unshare`、`setns`、`capset` 不能在普通 goroutine 中调用，要通过重新 exec 自己完成（coding-standards 第 6 节）。
- **shim 的 stdio 属于被代理的命令**：绝不向其中写诊断信息，否则会破坏命令输出和 MCP 的 JSON-RPC 流。
- **视图切换必须失败关闭**：Claude 进程在本地视图中继续运行，会把本地文件当作远端文件修改。
- **errno 保真**：远端的 errno 原样传回本地，不折叠成 `EIO`。
- **抽象 unix socket 没有权限保护**：必须校验 `SO_PEERCRED` 和会话 token。
- **不要对本地集合的挂载点及其祖先发送 FUSE entry 失效**：可能卸下 bind 挂载（filesystem 第 6 节）。
- **Claude Code 的行为大多没有文档**：兼容性测试失败时，先确认并更新 `docs/claude-code.md`，再改代码。

## 约定

- 文档用中文；代码、注释、标识符和提交信息用英文。
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

易变的信息引用它的权威来源，不要复制当前值。决策记录只在同时满足两个条件时保留：相关任务尚未完成，并且这个决策对今后的维护至关重要；否则删除。

代码使文档失效时，立即更新或删除受影响的内容，绝不把过时内容保留为「历史说明」。
