# 安全模型

## 信任边界

| 主体 | 信任 | 认证方式 |
|---|---|---|
| 本地用户 → 远端 tele server | 配对后完全信任：本地用户可以以远端用户身份执行任意命令、读写任意文件 | SS2022 PSK。配对时用一次性 token 交换，此后不再传输 |
| 重连的客户端 → 已有会话 | 只有建立该会话的客户端 | 会话密钥（见[可恢复会话层](transport.md#可恢复会话层)） |
| shim ↔ 会话主进程 | 只有同一会话中 Claude 启动的进程可以使用；shim 只把请求交给同一用户的会话主进程 | 双向用 `SO_PEERCRED` 校验 uid，会话主进程还校验会话 token |
| Claude → 本地 CONNECT 代理 | 只有这个会话的 Claude | 代理要求 `Proxy-Authorization`，密码随会话随机生成 |
| 远端 → 本地 | 远端返回的数据不可信：可能来自被入侵的主机 | 所有输入都校验；远端返回的路径只能写入本地的 scratch 目录 |

## 远程代码执行入口

exec 服务本质上就是远程代码执行的入口：

- 只接受通过认证的会话；未认证的连接得不到任何响应（见 [SS2022](transport.md#ss2022)）。
- tele server 以目标用户身份运行，不做任何提权，也不需要任何 capability。
- PSK 文件权限为 0600，不写入日志和错误信息，比较时使用常量时间比较。
- 泄露 PSK 等同于泄露远端用户的 shell 访问权限。`tele host rm` 删除本地的 PSK；远端用 `tele server uninstall` 或重新配对来吊销。

## 本地的会话主进程

- shim 连接的是**抽象** unix socket，它没有文件权限保护，同一个网络命名空间中的任何进程都能连接。所以既要用 `SO_PEERCRED` 校验对端 uid，也要校验会话 token。token 存放在会话目录中权限为 0600 的文件里，会话目录只在这个会话的远端视图和会话主进程中可见。
- 校验是**双向**的：抽象 socket 的名字没有属主，正在使用的 sid 又能从所有人可读的 `/proc/net/unix` 中看到。会话主进程退出后，另一个本地用户可以绑定同一个 `@tele-<sid>`，从而收到 shim 发来的 token 和 Claude 的 stdin、stdout、stderr，还能伪造退出状态。所以 shim 在发送任何内容之前，先用 `SO_PEERCRED` 确认监听方的 uid 与自己相同。
- 会话主进程在 userns 中持有 `CAP_SYS_ADMIN`，但这只在它自己创建的命名空间中有效。Claude 在切换视图后清空了全部 capability（见[命名空间的构建](filesystem.md#命名空间的构建)）。
- 视图切换失败时 Claude 必须立即退出，绝不能在本地视图中继续运行（见 [teleswitch](filesystem.md#teleswitch)）。

## 远端返回的数据

- scratch 回传：规范化路径、校验前缀、拒绝符号链接逃逸，单个文件有大小上限（见 [scratch 路径改写与回传](exec.md#scratch-路径改写与回传)）。
- 协议消息：先检查帧长再解码，校验枚举取值和路径（见[协议](coding-standards.md#协议)）。
- telefs 呈现的内容本来就是远端的，Claude 看到的就是远端用户能看到的。本地集合（`~/.claude*` 等）通过 bind 挂载盖在远端路径上，远端无法读写这些本地文件。
- 远端的项目配置（`.claude/settings.json` 中的 hooks、状态栏命令，`.mcp.json` 中的 server）启动的程序都经 shim 在远端执行。唯一在本地执行的是本地 exec 代理（见 [shim](exec.md#shim)），所以这份列表要保持最小，代理的程序也不能接受可以导致任意执行的参数。

## 流量特征

全随机字节流在部分审查环境中会被识别。可选的缓解手段见 [SS2022](transport.md#ss2022)。
