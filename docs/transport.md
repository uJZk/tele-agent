# 传输与会话层

```
exec / telefs / 变更推送 / MCP 代理 / 端口转发
   │ 多路复用流
┌──▼─────────────────────────────────────────────┐
│ 会话层：session_id，每个方向的 seq/ack，重放缓冲 │ ← 重连后续传，上层无感
└──┬─────────────────────────────────────────────┘
   │ SS2022 流 ×N（交互、元数据、大块数据分别走不同的连接）
  TCP
```

## 1. SS2022

- 库：[database64128/shadowsocks-go](https://github.com/database64128/shadowsocks-go)，方法 `2022-blake3-aes-256-gcm`。客户端用 `ss2022.StreamClientConfig{…}.NewStreamClient().DialStream`，服务端用 `StreamServerConfig{…}.NewStreamServer().HandleStream`。
- 目标地址固定为一个内部名称（`tele.internal:1`），tele 不做通用代理。
- SS2022 自带时间戳和 salt 重放过滤，所以**两端时钟误差必须在 30 秒以内**。`tele server install` 检查 NTP，`tele host ls` 和 `tele doctor` 报告时钟偏差。
- 未认证的连接按 RejectPolicy 处理：默认一直读到超时再关闭，不回任何字节，以抵抗主动探测。
- 「全随机字节流」在部分审查环境中会被识别。shadowsocks-go 支持用 `UnsafeRequestStreamPrefix` 加前缀伪装，作为可选的缓解手段；需要时可以再套一层 TLS 伪装。

## 2. 可恢复会话层

TCP 连接一断，里面的所有流都会立即失效，所以在 SS2022 之上、业务之下需要一层可恢复的会话层（思路与 mosh、Eternal Terminal、QUIC 连接迁移相同）。

1. **续传**：首次连接时协商 `session_id`。重连时双方交换各自「已收到的最大 seq」，重发对方还没确认的帧。接收方按 seq 去重，所以每一帧**恰好送达一次**，上层的流（包括 MCP 的长连接 stdio）完全无感。
2. **快速检测断线**：应用层心跳每 5 秒一次，15 秒没有响应就判定断开。同时监听本机 netlink 的地址和路由变化（切换 Wi-Fi、IP 变化），一旦发生就**立即主动重拨**，不等 TCP 超时。
3. **先建后断**：链路质量下降时，先建立新连接，把会话迁移过去，再关闭旧连接。
4. **重拨**：指数退避加抖动，每次重新解析 DNS，可以在多个端口或多个 endpoint 之间轮换。
5. **避免队头阻塞**：交互（exec 的 stdio）、元数据（telefs 的小请求）、大块数据（telefs 的读写）分别走**不同的 TCP 连接**，它们同属一个会话，这样一次大文件传输不会拖慢交互操作。

## 3. 各业务的断线语义

| 业务 | 断线期间 | 重连后 | 超过租约（默认 30 分钟） |
|---|---|---|---|
| Bash、hooks | 远端进程**继续运行**，输出缓存在服务端（有界，超出部分写入磁盘）；shim 阻塞等待 | 补发输出和退出状态 | 远端进程组先 SIGTERM 后 SIGKILL；shim 返回错误 |
| shim 被 SIGKILL（Claude 的超时或中断） | 会话主进程发现 shim 的连接关闭，**通过会话**通知远端结束进程组 | — | — |
| stdio MCP | 进程存活，消息排队 | 续传 | 进程结束，Claude 显示该 MCP 已断开 |
| telefs | 请求阻塞（类似 NFS 的 `hard`）；终端状态栏或日志显示「重连中」 | 续传 | 返回 `EIO` |
| 变更推送 | 服务端排队 | 续传；队列溢出时声明新 epoch，本地全量失效 | 全量失效 |

## 4. 会话层覆盖不到的情况

- **tele server 进程重启或远端主机重启**：服务端内存中的会话丢失。telefs 按路径重新解析节点并全量失效（[telefs.md](telefs.md) 第 5 节）；正在运行的命令失败；stdio MCP server 进程随之结束，Claude 显示该 MCP 已断开。
- **本地会话主进程崩溃**：Claude 随之退出，远端进程在租约到期后被清理。用 `tele <别名>[:<目录>] --resume` 恢复对话。

## 5. 测试

在 `net.Conn` 这一层注入断线、延迟、半开连接和乱序重连；端到端测试用 `tc netem` 模拟丢包，用 toxiproxy 模拟断流，用网络命名空间切换模拟 IP 变化。
