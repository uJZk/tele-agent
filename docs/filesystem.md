# 文件系统视图与命名空间

## 1. 原则

Claude 进程看到的**整个文件系统**（`/`）都是远端的，不只是项目目录。`Read /etc/nginx/nginx.conf`、`Read /tmp/out.txt`、`Read ~/.bashrc` 读到的都是远端内容，和远端 Bash 看到的一致。

Claude Code 是本地程序，启动和运行时需要的文件原本都得来自本机。通过「先在本地加载、再切换到远端视图」、本地 CONNECT 代理和 shim，这些需求大多被消除。最后只剩 Claude 自己的配置和凭证、托管策略，以及进程自身要用的 `/proc`、`/sys`、`/dev` 留在本地，下文称为**本地集合**。

远端的根通过 telefs 提供，见 [telefs.md](telefs.md)。

## 2. 本地集合

Claude 在项目之外会访问这些路径：安装目录（bun 通过 `/proc/self/exe` 读取内嵌的 JS）；动态链接器、`/etc/ld.so.cache` 和 libc 等动态库（运行时还会 dlopen libgcc_s 等）；DNS 相关的 `/etc/resolv.conf`、`/etc/hosts`、`/etc/host.conf`、`/etc/nsswitch.conf`；CA 证书目录；`/etc/claude-code/`；`~/.claude*`；以及 exec 的 `git`、`rg`、`uname`、`/bin/sh`。

如果把这些都作为本地例外盖在远端视图上，例外就太多了。所以按类别逐一消除：

| 类别 | 消除办法 | 结果 |
|---|---|---|
| Claude 二进制与动态库 | **先在本地视图中加载，再切换**：Claude 在本地 mountns 中 exec，动态链接器映射完所有 `DT_NEEDED` 库之后、`main` 之前，由预加载库把整个进程切换到远端视图（第 5 节）。之后 `/proc/self/exe` 走的是 magic link，与路径无关，bun 读取内嵌 JS 不受影响。运行时才 dlopen 的库（例如 libgcc_s）在切换前预先加载 | 不需要本地例外 |
| DNS | 会话主进程在本地回环地址上提供 **CONNECT 代理**，并设置 `HTTPS_PROXY`、`HTTP_PROXY`；用户原有的代理串联在它后面。Claude 自己不做 DNS 解析，由代理在本地视图中完成 | 不需要本地例外 |
| CA 证书 | **使用本地的 CA**：Claude 的 TLS 连接经本地代理从本机网络出站，信任关系应当与本地网络一致（例如公司的 HTTPS 中间人 CA），而远端可能根本没有装 `ca-certificates`，或者版本很旧。启动时把本地系统 CA 和用户原有的 `NODE_EXTRA_CA_CERTS`、`SSL_CERT_FILE` 合并成 `<sess>/ca-bundle.pem`，再用 `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS` 指向它。**不** bind 到 `/etc/ssl`，所以远端视图中的 `/etc/ssl/certs` 仍然是远端的 | 用本地 CA，但不增加本地例外 |
| `git`、`rg`、`uname` | `PATH` 中只有 `<sess>/bin` 里的转发 shim | 在远端执行 |
| `/bin/sh`（`shell: true` 的 spawn 固定使用它） | 替换为 tele 的 `sh` shim（[exec.md](exec.md) 第 1 节） | 语义上等同于远端的 sh |
| 必须在本地运行的程序（例如 `ps`，它要看到本地进程） | `PATH` 中放**本地 exec 代理**（[exec.md](exec.md) 第 1 节） | 在本地执行 |

**最终的本地集合**（在远端视图中通过 bind 挂载可见）：

| 路径 | 原因 | 与远端的冲突 |
|---|---|---|
| `$HOME/.claude/`、`$HOME/.claude.json` | Claude 自己的凭证、设置和会话历史 | 只有远端用户自己也用 Claude Code 时，才会遮住远端的同名路径 |
| `/etc/claude-code/` | 企业托管策略必须来自本机 | 远端通常不存在 |
| `/proc`、`/sys`、`/dev` | Claude 进程自身要用（`/proc/self` 等） | 实际上不构成例外：Agent 通过 Bash 或 Grep/Glob 访问这些路径时都在远端执行；只有 Read/Write/Edit 直接打开它们时看到的是本地内容，这种用法很少 |
| `/bin/sh` | tele 的 `sh` shim | 语义上等同于远端的 sh |
| `/.tele/<sid>/` | 会话目录 | 远端不存在该路径 |

`/etc/hosts`、`/etc/resolv.conf`、`/usr/lib/...`、`/lib64/ld-linux...` 这些路径在 Claude 看来**都是远端的**。

## 3. HOME

- Claude 进程的 `HOME` 设为**远端用户的家目录路径**（例如 `/home/bob`）。这样模型写 `~` 时，含义与远端 Bash 中的 `~` 一致。
- 本地的 `~/.claude` 和 `~/.claude.json` bind 挂载到这个 `HOME` 下的对应位置（`/home/bob/.claude` → 本地的 `/home/alice/.claude`），Claude 通过 `HOME` 找到自己的配置和凭证。

## 4. 命名空间的构建

1. `tele` 以 `CLONE_NEWUSER|CLONE_NEWNS` 重新 exec 自己，uid/gid 映射为自身，并带上 ambient `CAP_SYS_ADMIN`，成为**会话主进程**。它始终停留在**本地视图**：CONNECT 代理和本地 exec 代理在本地视图中工作，telefs 的本地主机后端也从这里读取本地文件。
2. 会话主进程用 `DirectMountStrict` 在本地的一个私有目录挂载 telefs，内容是远端的 `/`。telefs 为本地集合中的路径合成挂载点占位节点。
3. **准备远端视图**：一个辅助子进程执行 `unshare(CLONE_NEWNS)`，把本地集合 bind 挂载到占位节点上，rbind `/proc`、`/sys`、`/dev`，然后 `pivot_root` 到 telefs，再 `stat /`（原因见第 6 节）。会话主进程通过 `/proc/<pid>/ns/mnt` 持有这个 mountns 的 fd，辅助进程退出后命名空间依然存在。
4. **启动 Claude**：在**本地视图**中 exec Claude，保留 ambient `CAP_SYS_ADMIN` 和 `CAP_SYS_CHROOT`（`setns` 需要这两个），设置 `LD_PRELOAD=<sess>/lib/teleswitch.so`，并通过继承的 fd 把远端视图的 mountns 交给它。动态链接器在本地视图中完成所有库的映射。
5. **切换视图**：teleswitch 的构造函数在 `main` 之前依次执行：预加载运行时库 → `setns(mntns_fd, CLONE_NEWNS)` → `chdir(<工作目录>)` → 关闭继承的 fd → 从环境中清除 `LD_PRELOAD` 和 `TELE_*`，让子进程不再继承 → `PR_CAP_AMBIENT_CLEAR_ALL`，并用 `capset` 清空全部 capability。此后 Claude 以普通权限运行在远端视图中。
6. shim 通过**抽象 unix socket** 与会话主进程通信。抽象 socket 属于网络命名空间，不依赖文件路径，所以在两种视图中都能访问。

## 5. teleswitch

teleswitch 是一个用 C 写的小共享库。Go 运行时是多线程的，而 `setns(CLONE_NEWNS)` 要求调用进程不与其它线程共享文件系统信息，所以这一步只能在 Claude 的 `main` 之前、进程还是单线程时，由预加载库完成。

它不链接 libc，只使用原始系统调用（[coding-standards.md](coding-standards.md) 第 9 节），因此同时适用于 glibc 和 musl 版本的 Claude。它在构建时编译并嵌入 `tele`，运行时释放到 `<sess>/lib/`，所以对外发布仍然只有一个文件。

**必须失败关闭**：任何一步失败，Claude 进程都要立即退出。在本地视图中继续运行的 Claude 会把本地文件当作远端文件修改。

## 6. FUSE 与 userns 的已知陷阱

| 现象 | 原因 | 对策 |
|---|---|---|
| 挂载时 EPERM | `/dev/fuse` 不可读写（一些容器里是 0600，常规发行版是 0666） | `tele doctor` 检查 |
| 读正常，create 返回 EACCES | 内核把 FUSE 根 inode 的属主初始化为 uid 0，而 uid 0 在 userns 中没有映射（VFS 的 `HAS_UNMAPPED_ID` 检查） | 挂载后立即 `stat` 挂载点，让内核通过 GETATTR 刷新根 inode 的属主 |
| `BACKING_OPEN` 返回 EPERM | FUSE passthrough 需要**初始**用户命名空间中的 `CAP_SYS_ADMIN` | 不使用 passthrough（telefs 的文件本来就在远端） |
| 子进程仍然带着 `CAP_SYS_ADMIN` | ambient capability 会被 exec 继承 | 在 teleswitch 中清除（第 4 节第 5 步） |
| 以 root 运行时，子进程在 exec 后又获得全部 capability | uid 0 在 exec 时会重新获得 capability，清空 ambient 集合对它无效 | 测试要以普通用户运行，才能验证无特权语义 |
| 远端视图中的 bind 挂载被意外卸下（待验证） | FUSE 的 entry 失效通知会对 dentry 调用 `d_invalidate`，而 `d_invalidate` 会卸下挂在该 dentry 及其子孙上的所有挂载 | telefs 不对本地集合的挂载点及其祖先目录发送 entry 失效；这些节点的 LOOKUP 必须始终返回同一个 inode（[telefs.md](telefs.md) 第 4 节） |

**兼容性**：Ubuntu 23.10 及以后的版本默认 `kernel.apparmor_restrict_unprivileged_userns=1`，需要随包附带一个授予 `userns,` 的 AppArmor profile；`user.max_user_namespaces=0` 的系统无法使用 tele。两者都由 `tele doctor` 检查（[cli.md](cli.md) 第 5 节）。

## 7. 待验证的假设

下面的假设决定了「先本地加载、再切换视图」能否成立。实现前必须逐条验证，验证通过后把结论并入上文并删除本节对应条目。

| 假设 | 不成立时的退路 |
|---|---|
| 在 `main` 之前，bun 仍然是单线程的（`setns(CLONE_NEWNS)` 要求不与其它线程共享 fs 结构） | 回到「bind 挂载动态库和 DNS 文件」的做法，代价是本地例外变多 |
| 切换之后，Claude 不再 dlopen 或打开其它本地运行时文件（用 `strace` 对切换后的访问做差异比对；telefs 记录 Claude 对疑似运行时文件（`*.so*`、`/etc/ssl` 等）的访问供排查） | 在切换前预先加载；实在不行，再加入本地集合 |
| 本地 uid 在远端的 `/etc/passwd` 中可能不存在，但 `os.userInfo()` 等调用不受影响，或者 `USER`、`HOME` 环境变量足以兜底 | 在远端视图中合成 passwd 条目 |
| Claude 的所有出站 HTTP（API、WebFetch、遥测、OAuth 刷新）都遵循代理。不走代理的连接会在远端视图中做 DNS 解析，从而失败 | 把本地的 DNS 配置文件加入本地集合 |
| 只靠 `SSL_CERT_FILE` 和 `NODE_EXTRA_CA_CERTS`，bun 就会使用 `<sess>/ca-bundle.pem`，不再依赖系统证书目录 | 把本地证书目录 bind 到 `/etc/ssl` 等路径，代价是多一个本地例外 |
| `pivot_root` 到 FUSE 根，以及在嵌套的 mountns 中做 bind 挂载，都可以在 userns 中完成 | 无（这是方案的前提） |
| entry 失效会卸下挂载点（第 6 节最后一行） | 如果不会，就取消对应的限制 |
