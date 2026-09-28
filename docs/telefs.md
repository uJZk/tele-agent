# telefs

telefs 是 tele 自己的 FUSE 文件系统：本地由会话主进程提供 FUSE 服务，请求经会话通道转发给远端的 tele server，由它以目标用户身份访问远端文件系统。它呈现的是远端的整个 `/`（见 [filesystem.md](filesystem.md)）。

## 为什么需要挂载

命令类操作（Bash、hooks、stdio MCP、`rg`、`git`）都已经转发到远端，但 Claude Code 还有一大块功能是**进程内**的文件系统调用，不启动任何命令：

| 进程内的文件访问 | 没有挂载时的后果 |
|---|---|
| Read、Write、Edit、NotebookEdit | 读写的是本地磁盘，看不到远端文件 |
| 启动时加载项目的 `CLAUDE.md`、`.claude/settings*.json`、`.mcp.json`、`.claude/{skills,commands,agents}` | 项目配置、hooks 定义和技能全部丢失 |
| Bash spawn 时的 `cwd`，执行后读取 cwd 文件、检查目录是否存在 | 本地不存在该目录，spawn 报 ENOENT，或者 cwd 被重置回项目根 |
| 编辑前必须先读、判断文件是否被外部修改（基于 mtime） | 无法判断，Edit 被拒绝或误判 |
| checkpoint 与 `/rewind` | 回滚的是本地文件，远端不受影响 |
| `@` 文件引用与补全、读取图片和 PDF、git 状态上下文 | 失效或出错 |

不挂载的替代做法是禁用内置文件工具，改用 MCP 提供远程版本，代价是失去上表中的全部原生行为，所以不采用。

**负载很轻**：搜索、构建、测试、git 这类重负载都在远端直接执行，telefs 只承担 Claude 进程自身的文件访问，主要是读写单个文件和启动时加载配置。

## 组成

- **本地**：go-fuse v2，由会话主进程在 userns 中用 `DirectMountStrict` 挂载，不需要 fusermount。每个 FUSE 请求占用一条多路复用流。本地集合中的路径由合成的占位节点提供。
- **协议**：FUSE 操作一一映射为 RPC，消息定义在 `internal/proto`。
- **远端**：以目标用户身份访问文件。用户无权读取的文件（例如 `/etc/shadow`）返回 EACCES，与远端 Bash 的行为一致。errno 原样传回本地。
- **标识**：远端文件以 (dev, ino) 标识。inode 号在文件删除后会被复用，所以按路径重新解析节点时，标识不一致的节点按另一个文件处理。
- `/proc`、`/sys`、`/dev` 不从远端代理，因为远端视图中它们是本地的真实挂载。远端的进程信息请通过 Bash 查看。
- **排查辅助**：telefs 在 debug 日志中记录 Claude 进程对疑似运行时文件（`*.so*`、`/etc/ssl` 下的路径）的访问，用于发现新版本 Claude 在切换视图后仍然读取本地运行时文件的情况。

## 属主

telefs 把**所有**文件的属主都呈现为本地用户的 uid 和 gid：

- userns 只把本地 uid 映射为它自己。属主没有映射的 inode，VFS 一律拒绝写入（`HAS_UNMAPPED_ID`），即使远端允许。
- 挂载时不使用 `default_permissions`，内核不做权限检查。真正的检查在远端以目标用户身份进行；`access(2)` 也转发到远端判断。

代价是 Claude 进程内 `stat` 看到的属主不是远端的真实属主。需要真实属主时，在远端执行命令（例如 `ls -l`）。

## 一致性

1. **exec 屏障**：远端命令结束时，服务端先推送这条命令执行期间产生的变更，**然后**才返回退出状态；本地执行完对应的失效通知之后，shim 才返回（见 [exec 屏障](exec.md#exec-屏障)）。这保证了最常见的顺序「Bash 改了文件 → 紧接着 Read」一定读到新内容，Claude 基于 mtime 的修改检测也因此准确。
2. **后台变更**（后台进程、hooks、MCP server 造成的变更）异步推送。
3. 因为有推送，内核的 attr 和 entry 缓存可以使用较长的 TTL。推送通道断开时退回短 TTL；服务端丢失了事件（例如队列溢出）时，声明新的 **epoch**，本地对整棵树做全量失效。
4. 写入直接透传，fsync 和 close 时确认远端已落盘。不开 writeback cache，保证远端命令立即能看到写入。

## 变更监视

- 整个根目录无法全部用 inotify 监视，所以**按需注册**：服务端处理某个目录下的 LOOKUP 或 READDIR 时，对这个目录注册 inotify watch；本地发来这个目录的 FORGET 时注销。watch 的数量随 Claude 的工作集增长，而不是随文件系统的大小增长。
- 有 root 权限时可以改用 fanotify 的 `FAN_MARK_FILESYSTEM`，不受数量限制。
- 仍然需要调高远端的 `fs.inotify.max_user_watches`，由 `tele server install` 检查（见[预检与修复策略](cli.md#预检与修复策略)）。
- **不能对挂载点发送 entry 失效**：本地集合的挂载点及其祖先目录（例如 `HOME`），即使远端报告了变更，也只能做 attr 或内容失效，不能做 entry 失效；这些节点的 LOOKUP 必须始终返回同一个 inode。原因见[已知陷阱](filesystem.md#已知陷阱)。

## 断线与恢复

- 会话层保证请求和应答恰好送达一次（见[可恢复会话层](transport.md#可恢复会话层)），telefs 不在上层重试。
- 断线期间请求**阻塞**（类似 NFS 的 `hard` 挂载），超过会话租约后返回 `EIO`，避免 Claude 永远卡住。
- 远端服务进程重启后会话丢失。进行中的请求返回 `EIO`，不重放非幂等操作。本地按路径重新解析节点，并做全量失效。
