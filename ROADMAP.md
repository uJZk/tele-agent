# Roadmap

尚未完成、但已经决定要做的工作。完成一项就删除一项；设计与约定写进 `docs/`，不留在这里。

## 功能

- **命名空间与视图切换**：userns + mountns、`pivot_root` 到 telefs、本地集合的 bind 挂载、teleswitch（不链接 libc）及其 Go 侧封装（见[命名空间的构建](docs/filesystem.md#命名空间的构建)、[teleswitch](docs/filesystem.md#teleswitch)）。依赖它的有：会话主进程的完整启动流程、`tele <别名>` 主流程，以及 [docs/filesystem.md](docs/filesystem.md#待验证的假设) 和 [docs/claude-code.md](docs/claude-code.md#待验证的行为) 中切换视图之后才能验证的条目。
- **会话层断线期间缓冲命令输出**（有界，超出部分写入磁盘），见[进程与信号](docs/exec.md#进程与信号)；exec 服务目前只缓冲到流窗口为止。
- **验证 OAuth 令牌刷新遵循代理**：需要真实登录，兼容性测试的模拟 API 覆盖不到。

## 改进

- **scratch 上传以「服务端已写入」为准提交**
  - 现状：上传随命令的 ExecStart 发出，命令启动了才提交（[scratch 路径改写与回传](docs/exec.md#scratch-路径改写与回传)）。服务端在尝试启动之前就写入了上传的文件，所以命令不存在、工作目录不存在等启动失败时，文件其实已经写好，本地却回滚，下一条命令重传同样的内容（最多 `proto.ScratchTotalMax`）。
  - 做法：服务端在退出状态中明确报告上传已写入（只有 `checkStart` 拒绝的请求什么都没写），客户端据此提交。需要新增协议字段，并更新上面那一节的「上传以命令启动为准」。
  - 收益：省掉启动失败后的一次重传。启动失败少见，所以优先级低。
- **本地名字改由进程内的 fssvc 提供**
  - 现状：`.claude.json*` 这类本地名字由 `internal/telefs/local.go` 中另一套文件操作实现（见[组成](docs/telefs.md#组成)中的「本地名字」），它没有 xattr、`access`、`link`，errno 的兜底规则也是另写的。
  - 做法：会话主进程中再运行一个根目录为本地 HOME 的 `fssvc.Service`，经进程内的连接交给 telefs；telefs 的节点记录自己属于哪个后端，本地后端不做内核缓存、不接收变更推送，inode 编号与远端的分开。之后删除 `local.go` 中的文件操作。
  - 收益：本地名字与远端文件只有一套实现，语义一致，errno 和上限的修正只做一处，并直接受 fssvc 测试的保护。现有实现已经覆盖 Claude 对这些名字的全部操作，所以在本地集合增加新名字，或本地节点出现与远端不一致的问题时再做。
