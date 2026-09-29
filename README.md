# tele

tele 让 Claude Code 在本地运行，而它看到和操作的「世界」在一台远端主机上：Bash、hooks、stdio MCP server 都在远端执行，整个文件系统也是远端的。凭证、会话历史和 `~/.claude` 留在本地，远端不需要安装 Node 或 Claude Code。

```bash
tele dev                  # 在远端家目录启动 Claude
tele dev:proj --resume    # 工作目录 ~/proj；别名之后的参数原样传给 claude
```

## 特点

- **使用体验与 `claude` 一致**：`tele <别名>[:<目录>] [claude 参数...]`，不修改 Claude Code，也不拦截它的函数或系统调用。
- **两端都不需要 root**：本地用非特权 user namespace 和 FUSE 构建远端视图，远端服务以普通用户身份运行。
- **断线自动恢复**：SS2022 over TCP 之上有一层可恢复会话层，网络断开或切换时 Claude 侧无感。
- **单一静态二进制**：本地和远端用同一个 `tele`。

设计目标、非目标和结构见 [docs/architecture.md](docs/architecture.md)。

## 要求

- 两端都是 Linux（不支持 macOS 和 Windows）。
- 本地：`/dev/fuse` 可用，非特权 user namespace 可用，已安装 Claude Code。`tele doctor` 会检查这些条件，并在需要时给出修复步骤（例如 Ubuntu 上的 AppArmor 限制）。
- 两端时钟误差在 30 秒以内（SS2022 的要求）。
- 远端最好有 `bash`、`rg`、`git`，缺少时对应的 Claude 功能会失败。
- 不能与 Claude Code 自带的 bubblewrap 沙箱同时使用。

## 安装与配对

从 GitHub Releases 下载对应架构的 `tele-<版本>-linux-<架构>.tar.gz`，用 `SHA256SUMS` 校验后，把 `tele` 放到 `~/.local/bin/tele`。文件名必须是 `tele`：它按 `argv[0]` 分派，别的名字会被当作 shim。

远端能用 SSH 登录、且与本地架构相同时，一步完成：

```bash
tele host add myhost --ssh user@203.0.113.5
```

否则手动配对：

```bash
# 本地：生成 PSK，打印配对串 tele1:…
tele host add myhost --endpoint 203.0.113.5:8443

# 远端，以 tele 要运行的用户身份：提示时粘贴配对串，最后打印回执串 tele1r:…
tele server install

# 本地
tele host confirm myhost 'tele1r:…'
```

配对串包含 PSK，拿到它就能以远端用户身份执行任意命令，不要写在命令行或日志里。吊销要在远端执行 `tele server uninstall` 或重新配对，`tele host rm` 只删除本地的凭据。

完整的命令行、预检与修复策略、配置文件位置见 [docs/cli.md](docs/cli.md)，信任边界见 [docs/security.md](docs/security.md)。

## 从源码构建

```bash
make build        # 生成 bin/tele（需要 Go 与 C 编译器）
make check        # lint 与单元测试
```

其它目标和测试方法见 [CLAUDE.md](CLAUDE.md)，编码规范见 [docs/coding-standards.md](docs/coding-standards.md)。

## 文档

| 文档 | 内容 |
|---|---|
| [architecture.md](docs/architecture.md) | 目标、非目标与关键设计决策 |
| [cli.md](docs/cli.md) | 命令行、启动流程、安装与预检 |
| [claude-code.md](docs/claude-code.md) | tele 依赖的 Claude Code 行为及其验证方法 |
| [filesystem.md](docs/filesystem.md) | Claude 进程看到的文件系统视图 |
| [telefs.md](docs/telefs.md) | 远端文件系统的 FUSE 实现 |
| [exec.md](docs/exec.md) | 命令的远程执行与 shim |
| [transport.md](docs/transport.md) | 传输与可恢复会话层 |
| [security.md](docs/security.md) | 信任边界与安全约束 |

## 许可证

[AGPL-3.0](LICENSE)。依赖的 shadowsocks-go 是 AGPL-3.0，整个项目因此采用 AGPL-3.0；每个发布都附带包含全部依赖的源码归档。
