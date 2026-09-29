# Roadmap

尚未完成、但已经决定要做的工作。完成一项就删除一项；设计与约定写进 `docs/`，不留在这里。

## 功能

- **命名空间与视图切换**：userns + mountns、`pivot_root` 到 telefs、本地集合的 bind 挂载、teleswitch（不链接 libc）及其 Go 侧封装（见[命名空间的构建](docs/filesystem.md#命名空间的构建)、[teleswitch](docs/filesystem.md#teleswitch)）。依赖它的有：会话主进程的完整启动流程、`tele <别名>` 主流程，以及 [docs/filesystem.md](docs/filesystem.md#待验证的假设) 和 [docs/claude-code.md](docs/claude-code.md#待验证的行为) 中切换视图之后才能验证的条目。
- **会话层断线期间缓冲命令输出**（有界，超出部分写入磁盘），见[进程与信号](docs/exec.md#进程与信号)；exec 服务目前只缓冲到流窗口为止。
