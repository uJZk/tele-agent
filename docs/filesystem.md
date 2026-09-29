# 文件系统视图与命名空间

## 原则

Claude 进程看到的**整个文件系统**（`/`）都是远端的，不只是项目目录。`Read /etc/nginx/nginx.conf`、`Read /tmp/out.txt`、`Read ~/.bashrc` 读到的都是远端内容，和远端 Bash 看到的一致。

Claude Code 是本地程序，启动和运行时需要的文件原本都得来自本机。通过「先在本地加载、再切换到远端视图」、本地 CONNECT 代理和 shim，这些需求大多被消除。最后只剩 Claude 自己的配置和凭证、托管策略，以及进程自身要用的 `/proc`、`/sys`、`/dev` 留在本地，下文称为**本地集合**。

远端的根通过 telefs 提供，见 [telefs.md](telefs.md)。

## 本地集合

Claude 在项目之外需要访问的路径可以分为几类：自身的二进制和动态库、DNS 配置、CA 证书、托管策略（`/etc/claude-code/`）、自己的配置（`~/.claude*`），以及它按名字启动的外部程序。具体路径随 Claude Code 版本变化，以 `strace` 的观察结果为准（见[验证方法](claude-code.md#验证方法)）。

如果把这些都作为本地例外盖在远端视图上，例外就太多了。所以按类别逐一消除：

| 类别 | 消除办法 | 结果 |
|---|---|---|
| Claude 二进制与动态库 | **先在本地视图中加载，再切换**：Claude 在本地 mountns 中 exec，动态链接器映射完所有 `DT_NEEDED` 库之后、`main` 之前，由预加载库把整个进程切换到远端视图（见 [teleswitch](#teleswitch)）。之后 `/proc/self/exe` 走的是 magic link，与路径无关，bun 读取内嵌 JS 不受影响。运行时才 dlopen 的库（例如 libgcc_s）加入 `LD_PRELOAD`，在切换前就映射好 | 不需要本地例外 |
| DNS | 会话主进程在本地回环地址上提供 **CONNECT 代理**，并设置 `HTTPS_PROXY`、`HTTP_PROXY`；用户原有的代理串联在它后面（见下文的「CONNECT 代理」）。Claude 自己不做 DNS 解析，由代理在本地视图中完成 | 不需要本地例外 |
| CA 证书 | **使用本地的 CA**：Claude 的 TLS 连接经本地代理从本机网络出站，信任关系应当与本地网络一致（例如公司的 HTTPS 中间人 CA），而远端可能根本没有装 `ca-certificates`，或者版本很旧。启动时把本地系统 CA 和用户原有的 `NODE_EXTRA_CA_CERTS`、`SSL_CERT_FILE`、`SSL_CERT_DIR` 合并成 `<sess>/ca-bundle.pem`（规则见下文的「CA bundle」），再用 `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS` 指向它。Claude 的运行时还会扫描一个证书目录（见[代理与 CA](claude-code.md#代理与-ca)），所以 `SSL_CERT_DIR` 指向会话目录中的一个空目录：否则它会读远端的 `/etc/ssl/certs`，远端可以借此让 Claude 信任它放进去的 CA。**不** bind 到 `/etc/ssl`，所以远端视图中的 `/etc/ssl/certs` 仍然是远端的，但 Claude 不读它 | 用本地 CA，但不增加本地例外 |
| `git`、`rg`、`uname` | `PATH` 中只有 `<sess>/bin` 里的转发 shim | 在远端执行 |
| `/bin/sh`（`shell: true` 的 spawn 固定使用它） | 替换为 tele 的 `sh` shim（见 [shim](exec.md#shim)） | 语义上等同于远端的 sh |
| 必须在本地运行的程序（例如 `ps`，它要看到本地进程） | `PATH` 中放**本地 exec 代理**（见 [shim](exec.md#shim)） | 在本地执行 |

**CONNECT 代理**：

- 上游代理必须取自用户**原来的**环境，不能取自 tele 为 Claude 准备的环境：后者的 `HTTPS_PROXY` 指向代理自己。
- 上游代理的设置如果绕回了这个代理（例如残留的 `HTTPS_PROXY` 指向某个 tele 端口），每个请求都会递归转发给自己，直到进程耗尽 fd，FUSE 和会话通道也随之失效。所以代理转发的每个请求都带一个 `Via: <协议版本> tele-<随机>` 条目，随机部分每个会话不同；收到带有自己这个条目的请求时，在认证之前就以 508 拒绝，并提示修正或取消 `HTTPS_PROXY`、`HTTP_PROXY`。上游对 CONNECT 回 508 时，代理向客户端返回 502 和同样的提示。转发时保留客户端原有的 `Via` 条目，所以经过多个 tele 代理的环路也能发现。
- 这个 `Via` 条目对上游代理和明文 http 的源站可见。

**CA bundle**：

- 原则是**与本地直接运行 `claude` 时信任的 CA 一致**，不多也不少。
- 本地系统 CA 取 Go 系统证书文件列表中第一个可读的文件，**再加上**系统证书目录中的全部证书，去重。Claude 的运行时会读取这些目录，所以只放进 `/etc/ssl/certs` 而没有重新生成 bundle 文件的 CA，直接运行 `claude` 时是受信任的，tele 下也必须受信任。
- 用户的 `SSL_CERT_FILE`、`SSL_CERT_DIR`、`NODE_EXTRA_CA_CERTS` 是**追加**到系统 CA 上的；这与 Go 不同，Go 中 `SSL_CERT_FILE`、`SSL_CERT_DIR` 会替换系统列表。
- 用户指定的 CA 路径读不了时**只警告、不中止**，警告指明要修正或取消哪个变量。直接运行 `claude` 时，读不了 `NODE_EXTRA_CA_CERTS` 也只是警告（`ignoring extra certs from …`）；如果 tele 在这里拒绝启动，一个过期的环境变量就会造成「`claude` 能用、`tele` 不能用」。那个 CA 确实需要时，后续的 TLS 请求会失败，这条警告就是排查线索。

**最终的本地集合**（在远端视图中通过 bind 挂载可见）：

| 路径 | 原因 | 与远端的冲突 |
|---|---|---|
| `$HOME/.claude/` | Claude 自己的凭证、设置和会话历史 | 只有远端用户自己也用 Claude Code 时，才会遮住远端的同名路径 |
| `$HOME` 中以 `.claude.json` 开头的名字 | Claude 的全局配置，连同写它时用的临时文件和锁目录。不是 bind 挂载，而是由 telefs 直接交给本地 `HOME` 的同名条目（见 [~/.claude.json](claude-code.md#claudejson)） | 同上 |
| `/etc/claude-code/` | 企业托管策略必须来自本机 | 远端通常不存在 |
| `/proc`、`/sys`、`/dev` | Claude 进程自身要用（`/proc/self` 等） | 实际上不构成例外：Agent 通过 Bash 或 Grep/Glob 访问这些路径时都在远端执行；只有 Read/Write/Edit 直接打开它们时看到的是本地内容，这种用法很少 |
| `/bin/sh` | tele 的 `sh` shim | 语义上等同于远端的 sh |
| `/.tele/<sid>/` | 会话目录 | 远端不存在该路径 |

`/etc/hosts`、`/etc/resolv.conf`、`/usr/lib/...`、`/lib64/ld-linux...` 这些路径在 Claude 看来**都是远端的**。

## HOME

- Claude 进程的 `HOME` 设为**远端用户的家目录路径**（例如 `/home/bob`）。这样模型写 `~` 时，含义与远端 Bash 中的 `~` 一致。
- 本地的 `~/.claude` bind 挂载到这个 `HOME` 下的对应位置（`/home/bob/.claude` → 本地的 `/home/alice/.claude`）；`/home/bob/.claude.json*` 由 telefs 交给本地的 `/home/alice/.claude.json*`。Claude 通过 `HOME` 找到自己的配置和凭证。

## 命名空间的构建

1. `tele` 以 `CLONE_NEWUSER|CLONE_NEWNS` 重新 exec 自己，uid/gid 映射为自身，并带上 ambient `CAP_SYS_ADMIN` 和 `CAP_SYS_CHROOT`，成为**会话主进程**。它始终停留在**本地视图**：CONNECT 代理和本地 exec 代理在本地视图中工作。
2. 会话主进程先把自己的全部挂载设为私有，再用 `DirectMountStrict` 在本地的一个私有目录挂载 telefs，内容是远端的 `/`。telefs 为本地集合中的路径合成挂载点占位节点。会话目录在本地位于 `$XDG_CACHE_HOME/tele/s/<sid>`（默认 `~/.cache`），其中的 `tele` 是 tele 可执行文件的 bind 挂载，shim 都是指向它的相对符号链接，所以远端视图中的 shim 也能运行。
3. **准备远端视图**：一个辅助子进程在新的 mountns 中启动（Go 进程是多线程的，不能自己 `unshare(CLONE_NEWNS)`），把本地集合递归地 bind 挂载到占位节点上（包括 rbind `/proc`、`/sys`、`/dev`），然后 `pivot_root(".", ".")` 到 telefs 并分离旧的根，再 `stat /`（原因见[已知陷阱](#已知陷阱)）。会话主进程通过 `/proc/<pid>/ns/mnt` 持有这个 mountns 的 fd，辅助进程随后退出，命名空间依然存在。以上都在 userns 中完成，不需要任何特权。
4. **启动 Claude**：会话主进程启动一个**启动阶段**进程（tele 自身），它在新的 mountns 中用一个空的只读文件系统盖住 `/proc`，形成**启动视图**，然后 exec Claude。进程保留 ambient `CAP_SYS_ADMIN` 和 `CAP_SYS_CHROOT`（`setns` 需要这两个），通过继承的 fd 3 拿到远端视图的 mountns，并由启动阶段设置 `LD_PRELOAD=<本地会话目录>/lib/teleswitch.so`：启动视图是本地的，`/.tele/<sid>` 在那里不存在。启动阶段自己不带 `LD_PRELOAD`（库经 `TELE_LAUNCH_PRELOAD` 传给它），否则库会先切换启动阶段自己，并清掉它挂载所需的 capability。动态链接器在启动视图中完成所有库的映射。
5. **切换视图**：teleswitch 的构造函数在 `main` 之前依次执行：`setns(mntns_fd, CLONE_NEWNS)` → `chdir(<工作目录>)` → 确认 `TELE_SESSION` 指向的会话目录存在（它只存在于远端视图中） → 关闭继承的 fd → 从环境中清除 `LD_PRELOAD` 和只供 teleswitch 使用的 `TELE_SWITCH_*`，让子进程不再继承（shim 需要的 `TELE_SESSION` 保留） → `PR_CAP_AMBIENT_CLEAR_ALL`，并用 `capset` 清空全部 capability。此后 Claude 以普通权限运行在远端视图中。
6. shim 通过**抽象 unix socket** 与会话主进程通信。抽象 socket 属于网络命名空间，不依赖文件路径，所以在两种视图中都能访问。

## teleswitch

teleswitch 是一个用 C 写的小共享库。Go 运行时是多线程的，而 `setns(CLONE_NEWNS)` 要求调用进程不与其它线程共享文件系统信息，所以这一步只能在 Claude 的 `main` 之前、进程还是单线程时，由预加载库完成。

它不链接 libc，只使用原始系统调用（见 [C 代码](coding-standards.md#c-代码teleswitch)），因此同时适用于 glibc 和 musl 版本的 Claude。它在构建时编译并嵌入 `tele`，运行时释放到 `<sess>/lib/`，所以对外发布仍然只有一个文件。

**必须失败关闭**：任何一步失败，Claude 进程都要立即退出（退出码见 `teleswitch.ExitCode`，并在 stderr 写一行原因）。在本地视图中继续运行的 Claude 会把本地文件当作远端文件修改。

**清除环境变量时不改变数组长度**：被清除的条目改为指向空字符串，而不是把后面的条目前移。有的运行时（例如链接了 libc 的 Go 程序）沿初始栈上环境数组的 NULL 结尾向后找辅助向量，数组变短会让它们读到错误的辅助向量而崩溃。

## 已知陷阱

| 现象 | 原因 | 对策 |
|---|---|---|
| 挂载时 EPERM | `/dev/fuse` 不可读写（一些容器里是 0600，常规发行版是 0666） | `tele doctor` 检查 |
| 读正常，create 返回 EACCES | 内核把 FUSE 根 inode 的属主初始化为 uid 0，而 uid 0 在 userns 中没有映射（VFS 的 `HAS_UNMAPPED_ID` 检查） | 挂载后立即 `stat` 挂载点，让内核通过 GETATTR 刷新根 inode 的属主 |
| 对属主不是本地用户的文件写入时 EACCES，即使远端允许 | 同上：属主在 userns 中没有映射的 inode，VFS 一律拒绝写入 | telefs 把所有文件的属主呈现为本地用户（见 [属主](telefs.md#属主)） |
| `BACKING_OPEN` 返回 EPERM | FUSE passthrough 需要**初始**用户命名空间中的 `CAP_SYS_ADMIN` | 不使用 passthrough（telefs 的文件本来就在远端） |
| 子进程仍然带着 `CAP_SYS_ADMIN` | ambient capability 会被 exec 继承 | 在 teleswitch 中清除（见[命名空间的构建](#命名空间的构建)中的「切换视图」） |
| 以 root 运行时，子进程在 exec 后又获得全部 capability | uid 0 在 exec 时会重新获得 capability，清空 ambient 集合对它无效 | 测试要以普通用户运行，才能验证无特权语义 |
| bind 挂载落到了本地路径上 | 挂载目标路径上的绝对符号链接（例如远端的 `/bin` → `/usr/bin`）在 `pivot_root` 之前按本地的根解析 | 在远端根内解析挂载目标（`openat2` 的 `RESOLVE_IN_ROOT`），再挂载到解析出的 fd 上 |
| 预加载库无法映射 | 会话目录位于 `noexec` 的文件系统上（有的系统的 `/tmp`、`/run/user/<uid>`），动态链接器无法以可执行权限映射库 | 会话主进程拒绝 `noexec` 上的缓存目录，并提示用 `XDG_CACHE_HOME` 换到别处 |
| teleswitch 根本没有运行，Claude 却照常启动 | glibc 的动态链接器加载不了 `LD_PRELOAD` 中的库时（文件缺失、架构不符、无法映射），只打印 `cannot be preloaded … ignored`，然后照常运行程序。teleswitch 自身的失败关闭覆盖不到这种情况，Claude 会在本地视图中运行 | 不能只依赖 teleswitch 失败关闭：Claude 在启动视图中被 exec，那里的 `/proc` 是空的。Claude 的运行时在 `main` 之前就要读 `/proc/self/maps` 等文件，读不到时直接中止，所以预加载没有生效的 Claude 根本启动不了；切换到远端视图之后，`/proc` 是本地的真实挂载（见[视图切换](claude-code.md#视图切换)） |
| 远端视图中的 bind 挂载被意外卸下 | FUSE 的 entry 失效通知会对 dentry 调用 `d_invalidate`，而 `d_invalidate` 会卸下挂在该 dentry 及其子孙上的所有挂载 | telefs 不对本地集合的挂载点及其祖先目录发送 entry 失效；这些节点的 LOOKUP 必须始终返回同一个 inode（见[变更监视](telefs.md#变更监视)） |

**兼容性**：Ubuntu 23.10 及以后的版本默认 `kernel.apparmor_restrict_unprivileged_userns=1`，需要随包附带一个授予 `userns,` 的 AppArmor profile；`user.max_user_namespaces=0` 的系统无法使用 tele。两者都由 `tele doctor` 检查（见[预检与修复策略](cli.md#预检与修复策略)）。
