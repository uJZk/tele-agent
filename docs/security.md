# 安全模型

## 1. 信任边界

| 主体 | 信任 | 认证方式 |
|---|---|---|
| 本地用户 → 远端 tele server | 配对后完全信任：本地用户可以以远端用户身份执行任意命令、读写任意文件 | SS2022 PSK。配对时用一次性 token 交换，此后不再传输 |
| shim → 会话主进程 | 只有同一会话中 Claude 启动的进程可以使用 | `SO_PEERCRED` 校验 uid，加上通过环境变量交给 shim 的会话 token |
| 远端 → 本地 | 远端返回的数据不可信：可能来自被入侵的主机 | 所有输入都校验；远端返回的路径只能写入本地的 scratch 目录 |

## 2. exec 服务是远程代码执行的入口

- 只接受通过 PSK 认证的会话；未认证的连接得不到任何响应（[transport.md](transport.md) 第 1 节）。
- tele server 以目标用户身份运行，不做任何提权，也不需要任何 capability。
- PSK 文件权限为 0600，不写入日志和错误信息，比较时使用常量时间比较。
- 泄露 PSK 等同于泄露远端用户的 shell 访问权限。`tele host rm` 删除本地的 PSK；远端用 `tele server uninstall` 或重新配对来吊销。

## 3. 本地的会话主进程

- shim 连接的是**抽象** unix socket，它没有文件权限保护，同一个网络命名空间中的任何进程都能连接。所以既要校验对端 uid，也要校验会话 token；token 只存在于 Claude 进程及其子进程的环境中。
- 会话主进程在 userns 中持有 `CAP_SYS_ADMIN`，但这只在它自己创建的命名空间中有效。Claude 在切换视图后清空了全部 capability（[filesystem.md](filesystem.md) 第 4 节）。
- 视图切换失败时 Claude 必须立即退出，绝不能在本地视图中继续运行（[filesystem.md](filesystem.md) 第 5 节）。

## 4. 远端返回的数据

- scratch 回传：规范化路径、校验前缀、拒绝符号链接逃逸，单个文件不超过 1 MB（[exec.md](exec.md) 第 5 节）。
- 协议消息：先检查帧长再解码，校验枚举取值和路径（[coding-standards.md](coding-standards.md) 第 7 节）。
- telefs 呈现的内容本来就是远端的，Claude 看到的就是远端用户能看到的。本地集合（`~/.claude*` 等）通过 bind 挂载盖在远端路径上，远端无法读写这些本地文件。
- 远端的项目配置（`.claude/settings.json` 中的 hooks、状态栏命令，`.mcp.json` 中的 server）启动的程序都经 shim 在远端执行。唯一在本地执行的是本地 exec 代理（[exec.md](exec.md) 第 1 节），所以这份列表要保持最小，代理的程序也不能接受可以导致任意执行的参数。

## 5. 流量特征

全随机字节流在部分审查环境中会被识别。可选的缓解手段见 [transport.md](transport.md) 第 1 节。
