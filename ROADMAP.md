# Roadmap

尚未完成、但已经决定要做的工作。完成一项就删除一项；设计与约定写进 `docs/`，不留在这里。

## 功能

- **命名空间与视图切换**：userns + mountns、`pivot_root` 到 telefs、本地集合的 bind 挂载、teleswitch（不链接 libc）及其 Go 侧封装（见[命名空间的构建](docs/filesystem.md#命名空间的构建)、[teleswitch](docs/filesystem.md#teleswitch)）。依赖它的有：会话主进程的完整启动流程、`tele <别名>` 主流程，以及 [docs/filesystem.md](docs/filesystem.md#待验证的假设) 和 [docs/claude-code.md](docs/claude-code.md#待验证的行为) 中切换视图之后才能验证的条目。
- **会话层断线期间缓冲命令输出**（有界，超出部分写入磁盘），见[进程与信号](docs/exec.md#进程与信号)；exec 服务目前只缓冲到流窗口为止。
- **验证 OAuth 令牌刷新遵循代理**：需要真实登录，兼容性测试的模拟 API 覆盖不到。

## 改进

- **本地名字改由进程内的 fssvc 提供**
  - 现状：`.claude.json*` 这类本地名字由 `internal/telefs/local.go` 中另一套文件操作实现（见[组成](docs/telefs.md#组成)中的「本地名字」），它没有 xattr、`access`、`link`，errno 的兜底规则也是另写的。
  - 做法：会话主进程中再运行一个根目录为本地 HOME 的 `fssvc.Service`，经进程内的连接交给 telefs；telefs 的节点记录自己属于哪个后端，本地后端不做内核缓存、不接收变更推送，inode 编号与远端的分开。之后删除 `local.go` 中的文件操作。
  - 收益：本地名字与远端文件只有一套实现，语义一致，errno 和上限的修正只做一处，并直接受 fssvc 测试的保护。现有实现已经覆盖 Claude 对这些名字的全部操作，所以在本地集合增加新名字，或本地节点出现与远端不一致的问题时再做。
