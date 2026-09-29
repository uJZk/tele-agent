# 命令行、启动流程与安装

## 命令形式

```bash
tele [tele 选项] <别名>[:<目录>] [claude 参数...]

tele dev                  # 在远端家目录启动 Claude
tele dev:proj             # ~/proj（相对路径按远端家目录解析）
tele dev:/srv/app         # 绝对路径
tele dev:proj --resume    # 别名之后的参数原样传给 claude
tele dev -p "…"           # 同上
```

- **`<别名>[:<目录>]`** 沿用 scp、rsync 的 `host:path` 写法。省略目录时是**远端用户的家目录**；相对路径按远端家目录解析；末尾的 `/` 会被去掉。目录不存在时直接报错退出，不自动创建。这个目录就是 Claude 的工作目录，也是 Claude 识别项目的依据。
- **参数边界**：tele 自己的选项必须写在别名**之前**，别名之后的所有内容都原样传给 Claude Code。这与 `ssh [选项] host [命令]`、`docker run [选项] 镜像 [参数]` 的惯例一致，不需要 `--`，也不会和 claude 的参数冲突。
- **切分规则**：按**第一个** `:` 切分，所以别名中不能含 `:`，目录中可以含。
- **一个 tele 进程只对应一台主机**，运行期间不切换。要换主机，就退出后用另一个别名重新启动。

**tele 选项**：

| 选项 | 作用 |
|---|---|
| `--claude <路径>` | 使用的 Claude Code 可执行文件；默认按 `PATH` 查找 `claude` |
| `--debug` | 会话主进程的日志记录 debug 级别的信息 |
| `--log <文件>` | 会话主进程的日志另外追加到这个文件：会话目录中的日志随会话结束而删除 |
| `--endpoint <endpoint>` | 覆盖别名配置中的 endpoint |

**其它命令**：

| 命令 | 作用 |
|---|---|
| `tele host add <别名> …` / `tele host confirm` / `tele host ls` / `tele host rm` | 登记主机、配对，查看状态（连通性、RTT、时钟偏差、操作系统信息） |
| `tele doctor [--claude <路径>] [别名]` | 本地检查，指定别名时再检查到该主机的连通性和时钟偏差（见[预检与修复策略](#预检与修复策略)） |
| `tele server install` / `run` / `uninstall` | 远端服务。`install` 配对并安装（见[安装与配对](#安装与配对)）；`run` 在前台运行服务，日志写到 stderr（由 systemd 送进 journal），收到 SIGTERM 或 SIGINT 后结束所有会话再退出；`--listen` 覆盖配置中的监听地址；`uninstall` 按安装清单回滚，并删除配置以吊销 PSK |
| `tele version` | 打印版本：发布版本由构建时注入，开发构建显示 Go 工具链记录的 VCS 修订 |

所有子命令名都是保留字，不能用作主机别名。

## 启动流程

启动器分两个阶段：第 1 阶段解析命令行，找到 `claude` 并检查它的版本，然后在新的 userns + mountns 中重新 exec 自己，成为会话主进程（第 2 阶段）；它等待会话主进程，并以同样的方式结束（退出码或信号）。下面是会话主进程的步骤。终端的 SIGINT、SIGQUIT 本来就会送到整个前台进程组，所以两个阶段都只是接住它们，SIGTERM、SIGHUP 则转发给子进程。

1. 读取别名配置，建立会话（SS2022 + 会话层）。连接不上时报错退出，并提示运行 `tele doctor <别名>`。
2. 从服务端获取目标主机的信息：主机名、操作系统和发行版、内核、架构、远端用户、`$HOME`、登录 shell 及其 PATH；解析工作目录，并确认它存在。
3. 按[命名空间的构建](filesystem.md#命名空间的构建)构建命名空间：挂载 telefs（远端的 `/`），准备远端视图（本地集合、`HOME`）。
4. 生成系统提示词文件、shim 目录和 teleswitch 库，启动 CONNECT 代理，准备好[注入的环境](claude-code.md#注入的环境)。
5. 经启动阶段在启动视图中 exec `claude`，别名之后的参数原样透传；teleswitch 在 `main` 之前切换到远端视图，并 `chdir` 到工作目录。
6. Claude 退出后清理会话：停止 shim 服务（仍在运行的远端命令随之结束）、代理和变更推送，卸载 FUSE，删除会话目录，关闭会话；远端进程按租约规则处理（见[断线语义](transport.md#断线语义)）。会话主进程以 Claude 的退出状态结束。

**已知限制：会话历史不按主机隔离**。Claude 以 cwd 路径作为 `~/.claude/projects/` 下的项目键，不同主机上的相同路径会共用会话历史，`--resume` 时会一起列出。tele 不改变这一行为。

## 安装与配对

```bash
# 本地：生成 PSK 和一次性 token，别名进入「待确认」状态；打印配对串 tele1:… 和远端的步骤
tele host add myhost --endpoint 203.0.113.5:8443

# 远端，以 tele 要运行的用户身份（普通用户即可）：
# 从项目的 GitHub Releases 下载与远端架构对应的 tele-<版本>-linux-<架构>.tar.gz，用 SHA256SUMS 校验，
# 把其中的 tele 放到 ~/.local/bin/tele（文件名必须是 tele：它按 argv[0] 分派，别的名字会被当作 shim）
tele server install          # 提示时粘贴配对串；预检、写入配置、启动服务，最后打印回执串 tele1r:…

# 本地
tele host confirm myhost 'tele1r:…'
```

- **配对串就是密钥**：它包含 PSK，拿到它就能以远端用户身份执行任意命令。所以 `tele server install` 默认在终端里提示粘贴（不回显），而不是写在命令行上：命令行会留在 shell 历史中，运行期间还能被同一台机器上的其他用户从 `/proc/<pid>/cmdline` 读到。`--pair <配对串>` 只用于自动化，`--pair-file <文件>` 读取后删除该文件。
- **回执证明服务端装上的正是这次的配对串**：回执中有用 PSK 对一次性 token 计算的 HMAC，`tele host confirm` 核对它之后才解除「待确认」状态；待确认的别名不能用来启动会话。回执本身不含密钥。
- **一台服务端只有一个 PSK**：对已经配置过的服务端再次执行 `tele server install`，新的 PSK 会取代旧的，之前配对的客户端随之失效，所以 install 会先征得同意。同一个用户想在多台本地机器上使用同一个服务端时，可以把本地的主机文件（见[配置与状态文件](#配置与状态文件)）复制过去。
- **吊销**：`tele host rm` 只删除本地的 PSK；服务端仍然接受它，要在服务端执行 `tele server uninstall` 或重新配对才能吊销。
- 配对串中的端口是服务端监听的端口。服务端在 NAT 后面、对外端口与监听端口不同时，用 `tele server install --listen` 指定监听地址，`tele host confirm` 会提示两者不一致。

**`tele host add --ssh [user@]host`** 一步完成配对，远端不需要预先安装 tele：检查远端的架构与本地 tele 相同（不同时报错，改为手动安装），经 SSH 把本地的 tele 上传到 `~/.local/bin/tele`，把配对串经 SSH 的标准输入写入远端一个权限为 0600 的文件（不出现在任何命令行上），执行 `tele server install --pair-file`（本地有终端时分配终端，以便回答预检中需要同意的项目），从输出中取出回执并自动确认。省略 `--endpoint` 时，endpoint 是 SSH 目标的主机名加默认端口 8443。

远端服务默认以 `systemd --user` 单元 `tele-server.service` 运行，`ExecStart` 是 `~/.local/bin/tele server run`。从别处运行 `tele server install`（例如解压目录）时，它先把自己复制到 `~/.local/bin/tele`，免得单元指向一个随时会被删除的文件；只有当前用户无权写入的位置（例如发行版的软件包）才直接使用原路径。

对已经配置过的服务端不带配对串运行 `tele server install`（终端中直接回车），保留现有的 PSK，只重新预检和修复。回执在配置写入后打印，即使还有未通过的检查项：配对本身已经完成，服务端的问题修好后再 `tele host confirm` 也可以。

## 配置与状态文件

| 路径 | 内容 |
|---|---|
| `~/.config/tele/hosts/<别名>.json`（本地） | 主机别名的 endpoint 与凭据（0600）：SS2022 endpoint（`host:port`）用 PSK，`unix:<路径>` endpoint 用 token。`alternates` 列出同一服务端的其他 `host:port`（例如 IPv4 与 IPv6 地址、不同端口），会话层在它们之间轮换（见[可恢复会话层](transport.md#可恢复会话层)）；`tele host add` 多次给出 `--endpoint` 时，第一个是主 endpoint，其余进入 `alternates`。其他用户可读时拒绝使用 |
| `~/.config/tele/server.json`（远端） | 服务端的监听地址与 PSK（0600）。其他用户可读时拒绝启动 |
| `~/.config/tele/install-manifest.json`（远端） | 安装清单，见[预检与修复策略](#预检与修复策略)；被替换的文件备份在同一目录的 `backup/` 中 |
| `~/.config/systemd/user/tele-server.service`（远端） | 服务的 systemd 单元 |
| `~/.cache/tele/doctor-passed`（本地） | 本地检查已经通过的标记，见[预检与修复策略](#预检与修复策略)；删除它，下次启动会话时重新检查 |
| `/etc/apparmor.d/tele`（本地） | 允许 tele 创建 userns 的 AppArmor profile，只在需要时由 `tele doctor` 安装 |
| `~/.cache/tele/s/<sid>/`（远端） | 会话目录：scratch 文件、溢出到磁盘的命令输出 |

## 预检与修复策略

**原则**：先检查全部项目，再按类别处理，而不是一律报错或一律自动修改：

- 只影响当前用户、可以撤销的操作 → **自动执行**；
- 涉及系统范围或需要提权的操作 → **展示将要执行的确切命令，征得同意后执行**；
- 无法修复的问题 → **报错**，并给出原因和指引。

一律报错时，用户要逐条手动修复，安装体验差；一律自动修改，会在用户不知情时改动防火墙、sysctl、linger 这类系统设置，也会在非交互场景（CI、由 Claude 调用）下卡在提权提示上。

**流程**：

1. `tele server install`（以及本地的 `tele doctor`）先做**只读预检**，输出一张清单：✅ 通过 / 🔧 可自动修复 / 🔐 需同意或提权 / ❌ 无法修复 / ⚠️ 警告。
2. 🔧 项自动执行。
3. 🔐 项逐条展示**确切命令**，由用户确认（`[y/N]`）。
4. 最后重新预检，确认结果。
5. 存在 ❌ 项，或必需的 🔐 项没有得到同意时，以非零状态退出，并打印需要手动执行的命令。

**远端检查项**：

| 检查项 | 类别 | 处理 |
|---|---|---|
| 二进制与配置目录（`~/.local/bin`、`~/.config/tele`），PSK 文件权限 0600 | 🔧 | 直接创建和设置 |
| `systemd --user` 单元的安装、启用、启动 | 🔧 | 直接执行；没有 user manager 时降级为 ⚠️，改为提示前台运行或安装为系统服务 |
| linger（`loginctl enable-linger $USER`） | 🔐 | 没有 linger 时，用户登出后服务会停止。polkit 的 `set-self-linger` 在活跃会话中通常允许，在 SSH 等非活跃会话中可能需要认证。拒绝时降级为 ⚠️，并说明后果 |
| 监听端口可达（本机防火墙：firewalld、ufw、nft） | 🔐 | 生成对应前端的放行命令，经同意后用 sudo 执行；云安全组无法检测，只给出提示 |
| `fs.inotify.max_user_watches` 不足（按项目文件数估算） | 🔐 | 写入 `/etc/sysctl.d/90-tele.conf` 并执行 `sysctl --system`；拒绝时降级为 ⚠️：变更推送会退回短 TTL |
| 时钟同步（SS2022 要求误差在 30 秒以内） | NTP 未启用为 🔐 | 启用 `timedatectl set-ntp true` 需要同意。服务端没有参照时钟，偏差本身由客户端测量（见下面的本地检查项） |
| `bash`、`rg`、`git` 是否可用 | ⚠️ | 缺少时对应的 Claude 功能会失败（见 [shim](exec.md#shim)） |
| 内核版本、`/proc` 已挂载（telefs 服务端经 `/proc/self/fd` 操作文件，见[对象标识](telefs.md#对象标识)）、`/proc/sys/fs/inotify` 可用 | ❌ | 报错，并说明最低要求 |

**本地检查项**（`tele doctor`；首次运行 `tele <别名>` 时自动执行）：

| 检查项 | 类别 | 处理 |
|---|---|---|
| `/dev/fuse` 存在且可读写 | 权限不足为 🔐；不存在为 ❌ | 发行版默认是 0666；异常时给出 `modprobe fuse` 或 udev 规则的建议 |
| 非特权 userns 可用（`user.max_user_namespaces`、Ubuntu 的 AppArmor 限制） | 🔐 | 安装随包附带的 AppArmor profile（需要 sudo），而不是全局关闭限制；拒绝时报错 |
| Claude Code 版本在已验证列表中 | ⚠️ | 未验证的版本给出警告，但仍然允许运行 |
| 与目标主机的连通性和时钟偏差（指定别名时） | ⚠️ / ❌ | 偏差按 Hello 中服务端报告的时间估算：10 秒以上为 ⚠️，30 秒以上为 ❌。偏差超过 30 秒时 SS2022 握手本身就会失败，得不到服务端的时间，这时只能报告连接失败的可能原因和本地时钟是否同步 |

**通用约定**：

- **非交互**时（没有 TTY，例如在 CI 中，或由 Claude 在 Bash 里调用）**绝不提权**：🔧 项照常执行，🔐 项全部视为未同意，打印需要手动执行的命令。
- `--yes` 表示同意所有 🔐 项，用于自动化部署；`--check` 只做预检；`--print-commands` 只打印命令，不执行。
- 所有改动记入安装清单，`tele server uninstall` 据此按相反顺序逐项回滚：自己写入的文件删除，替换过的文件从备份恢复，🔐 项的回滚命令同样要征得同意。服务端配置从不备份，而且即使清单中没有它也会被删除，因为删除它就是吊销 PSK。
- 每一步都是幂等的，可以重复运行。检查项在修复前重新运行一次，因为前面的修复可能改变了它的状态（例如写入新的 PSK 后，正在运行的服务需要重启）。

**不明显的细节**：

- **inotify watch 的估算**：telefs 服务端按需对目录注册 watch（见[变更监视](telefs.md#变更监视)），需要量随 Claude 访问过的目录数增长。安装时还不知道项目，所以按家目录下的目录数估算（计数有时间上限），要求至少为目录数的 2 倍、不少于 65536；需要调高时至少写入 524288，给同一用户的其他程序（编辑器等）留出余量。
- **ufw 的规则需要 root 才能读取**，所以只认 tele 自己在清单中记录过的放行规则；用户已经手动放行时，重复执行 `ufw allow` 也是无害的。firewalld 的查询不需要 root，按实际规则判断。
- **没有 systemd user manager**（容器、没有 `pam_systemd` 的登录方式）时，服务、linger 两项都降级为 ⚠️，其余检查照常，配置照常写入。
- **本地检查在首次启动会话时自动执行**：只做预检，不修复；未通过时报告清单并提示运行 `tele doctor`，通过后在缓存目录留下标记，以后不再检查。
- **userns 的检查是实际尝试**：以启动会话主进程的方式创建 userns + mountns，并在其中修改挂载传播。只检查 sysctl 不够：Ubuntu 的 AppArmor 限制允许创建 userns，但会收回其中的 capability。
- **AppArmor profile 只放行这一个可执行文件**：profile 按 tele 可执行文件的绝对路径附着，内容是 `userns,`，随包附带的模板中路径是占位符，由 `tele doctor` 填入。不用通配路径（例如 `@{HOME}/.local/bin/tele`），否则任何用户把任意程序放到这个路径，都能绕过系统对非特权 userns 的限制。移动 tele 之后要重新运行 `tele doctor`。卸载：`sudo apparmor_parser -R /etc/apparmor.d/tele && sudo rm /etc/apparmor.d/tele`。
