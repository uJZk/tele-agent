# Roadmap

尚未完成、但已经决定要做的工作。完成一项就删除一项；设计与约定写进 `docs/`，不留在这里。

- **配对**：`tele host add/confirm/ls/rm`、`tele host add --ssh`（[安装与配对](docs/cli.md#安装与配对)）。
- **`tele server install/uninstall` 与 `tele doctor`**：预检、`systemd --user` 单元、安装清单与回滚、时钟偏差报告（[预检与修复策略](docs/cli.md#预检与修复策略)）。
- **端口转发**：`.mcp.json` 中指向 `localhost` 的 HTTP/SSE MCP（[端口转发](docs/exec.md#端口转发)）。
- **会话层**：netlink 触发的立即重拨、先建后断、endpoint 轮换、按流量类别分开的 TCP 连接（[可恢复会话层](docs/transport.md#可恢复会话层)）。
- **AppArmor profile**：随包附带，由 `tele doctor` 安装（[预检与修复策略](docs/cli.md#预检与修复策略)）。
- **发布**：按架构构建静态二进制、校验和，并附带对应源码（AGPL-3.0）。
