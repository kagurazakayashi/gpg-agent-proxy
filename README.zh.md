# gpg-agent-proxy

[English](README.md) | [简体中文](README.zh.md)

一个仅用于 Windows 的小桥接程序：把 Windows 上的 GnuPG `gpg-agent` 变成可以
被远程 Linux 主机通过 `ssh -R`（`RemoteForward`）使用的端点，让服务器上的
`git` 与 `gpg` 能用插在 Windows 机器上的 YubiKey（OpenPGP 卡）签名。私钥始终
不会离开 YubiKey。

## 为什么需要它

Windows 版 GnuPG 2.5 的 `gpg-agent` **没有文件 socket**：它监听 `127.0.0.1`
上的一个随机 TCP 端口，并且要求每个连上来的客户端先送出 **16 字节 nonce**
才会回应。端口号与 nonce 写在：

```
%LOCALAPPDATA%\gnupg\S.gpg-agent.extra
```

（第一行是端口号，紧随其后是 16 字节的原始 nonce。）

既然没有 socket 文件可转发，`RemoteForward <远端 socket> <本地 socket>` 这类
写法就无法生效。本程序改为监听 `127.0.0.1` 上的一个普通 TCP 端口，并在每个
连入的连接上自行连上真正的 agent、补送 nonce，然后双向搬运数据。

## 工作原理

```
 远端 Linux 主机                              Windows
+-----------------------+              +--------------------------------+
| git commit -S         |              | ssh   RemoteForward:           |
|   -> gpg              |              |   远端 /run/user/1000/gnupg/   |
|      -> $XDG_RUNTIME_DIR/gnupg/  <==>|   S.gpg-agent                  |
|         S.gpg-agent   |   隧道        |            ^                   |
|         (unix socket) |              |            |                   |
+-----------------------+              |   gpg-agent-proxy              |
                                       |   127.0.0.1:16688              |
                                       |            |  TCP + 16B nonce  |
                                       |            v                   |
                                       |   gpg-agent (GnuPG 2.5)        |
                                       |            |                   |
                                       |            v                   |
                                       |   YubiKey (OpenPGP 卡)         |
                                       +--------------------------------+
```

服务器一侧始终面对普通的 Unix socket，只有 Windows 这一端比较特殊，这个桥接
程序就是把这份特殊藏起来。

## 环境要求

- Windows，安装 GnuPG（Gpg4win）2.4 或更新版本，且 `gpg-agent` 正在运行。
- 一把 OpenPGP 密钥，签名子密钥在 YubiKey 上（或在 agent 中）。
- Windows 上有 OpenSSH 客户端（系统自带的
  `C:\Windows\System32\OpenSSH\ssh.exe`，或 Git for Windows 自带的那个）。
- 一台远程 Linux 主机，具备 `gnupg`、`git`，并且可以修改
  `/etc/ssh/sshd_config`。
- 若要自行编译，需要 Go 1.22 或更新版本。

## 构建

```sh
go build -o gpg-agent-proxy.exe .
```

可选的检查与更小的体积：

```sh
gofmt -l .                  # 不应该输出任何内容
go vet ./...                # 不应该输出任何内容
go build -trimpath -ldflags "-s -w" -o gpg-agent-proxy.exe .
```

仓库结构刻意保持精简：`main.go`（全部逻辑）与 `go.mod`，没有任何第三方依赖。

## 运行

```
gpg-agent-proxy.exe [参数]
```

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:16688` | 本地监听的 TCP 地址。ssh 配置里的 `RemoteForward` 目标必须与它一致。请保持在回环地址上。 |
| `-socket` | `%LOCALAPPDATA%\gnupg\S.gpg-agent.extra` | 要读取的 agent 端点文件。可用 `gpgconf --list-dirs agent-extra-socket` 确认真实路径。 |
| `-log` | 空 | 同时把控制台日志追加到该文件。留空表示只输出到控制台。 |

示例：

```bat
gpg-agent-proxy.exe
gpg-agent-proxy.exe -listen 127.0.0.1:16688 -log gpg-agent-proxy.log
```

启动后的预期输出（全部为英文，时间戳为本地时间）：

```
21:04:07 gpg-agent-proxy listening on 127.0.0.1:16688
21:04:07 upstream agent endpoint file: C:\Users\you\AppData\Local\gnupg\S.gpg-agent.extra
21:04:07 gpg-agent endpoint: 127.0.0.1:64457 (16-byte nonce)
21:04:07 press Ctrl+C to stop
```

使用隧道期间请让这个窗口保持打开。按 `Ctrl+C`（或关闭控制台）会优雅退出：
正在使用的连接会被断开，端口被释放，进程以退出码 0 结束。退出码 1 表示监听
失败（通常是已经有一个实例在运行）或日志文件无法打开；退出码 2 表示命令行
参数有误。

agent 端点是在**每个连接**上重新读取的，所以重启 `gpg-agent`（端口与 nonce
都会变）之后你不需要做任何事。

## Windows 本地配置

1. 确认 agent 在运行、并且能看到你的密钥：

   ```bat
   gpg-connect-agent /bye
   gpg --card-status
   gpgconf --list-dirs agent-extra-socket
   ```

2. 启动桥接程序并让它一直运行：

   ```bat
   gpg-agent-proxy.exe
   ```

   若想开机自动启动，可以把该 exe 的快捷方式放进 `shell:startup`。

3. 在 `%USERPROFILE%\.ssh\config` 中添加主机条目：

   ```
   Host myserver
       HostName 203.0.113.10
       User yashi
       IdentityFile ~/.ssh/id_ed25519
       RemoteForward /run/user/1000/gnupg/S.gpg-agent 127.0.0.1:16688
       StreamLocalBindUnlink yes
   ```

   需要留意的地方：

   - **左侧**是**远端**主机上的路径，不是 Windows 路径。请在服务器上执行
     `gpgconf --list-dirs agent-socket` 取得；多数发行版是
     `/run/user/<uid>/gnupg/S.gpg-agent`，把 `1000` 换成你的 uid。
   - **右侧**是本桥接程序提供的普通 TCP 地址。不要在这里填 Windows 路径，
     那正是无法生效的写法。
   - `StreamLocalBindUnlink yes` 让 ssh 可以替换服务器上由早前会话遗留的
     失效 socket。
   - 无论使用 Git for Windows 的 `ssh` 还是系统自带的 `ssh.exe`，这份配置都
     适用。

4. 不发起连接，仅验证 ssh 能否正确解析配置：

   ```bat
   ssh -G myserver | findstr /i remoteforward
   ```

   预期输出：

   ```
   remoteforward /run/user/1000/gnupg/S.gpg-agent [127.0.0.1]:16688
   ```

## Linux 远端配置

在你要签名的服务器上做一次即可。

1. 先确认远端的 `gpg` 会使用哪个 socket 路径，并把它填到 `RemoteForward` 的
   左侧：

   ```sh
   echo "$XDG_RUNTIME_DIR"
   gpgconf --list-dirs agent-socket
   ```

2. 导入你的公钥。私钥（YubiKey 上的子密钥）**不要**复制过去，只需要公钥，
   好让远端 `gpg` 知道该向哪把卡上密钥请求签名：

   ```sh
   gpg --import public-key.asc
   gpg -K --keyid-format long     # 签名密钥应显示 card-no: 或 #
   ```

3. 让服务器自己的 `gpg-agent` 让开，否则它会一直占着这个 socket，ssh 无法
   绑定转送过来的那个：

   ```sh
   gpgconf --kill gpg-agent
   echo no-autostart >> ~/.gnupg/gpg-agent.conf

   # 使用 systemd 的发行版还要阻止它被 socket 激活：
   systemctl --user mask --now gpg-agent.socket gpg-agent-ssh.socket \
       gpg-agent-extra.socket gpg-agent-browser.socket
   ```

4. 允许 sshd 替换遗留 socket（需要 root，执行一次）：

   ```sh
   echo 'StreamLocalBindUnlink yes' >> /etc/ssh/sshd_config
   systemctl restart sshd
   ```

5. 从 Windows 重新连接。登录过程中 ssh 会创建远端 socket。如果出现
   `remote port forwarding failed for listen path ...` 之类的警告，说明远端
   agent 仍占着它：回到第 3 步处理后重连。

## 验证

在 ssh 会话里问一句"回答我的 agent 是哪个版本"：

```sh
gpg-connect-agent 'GETINFO version' /bye
```

- 输出 `D 2.5.18`（你 Windows 版 GnuPG 的版本）说明隧道已经打通。
- 如果显示的是服务器自己的版本，说明转送没有生效：检查 ssh 的警告、远端
  路径，以及上面的第 4 步。

其他有用的检查：

```sh
ls -l "$XDG_RUNTIME_DIR/gnupg/"      # S.gpg-agent 应为 socket（srw-------）
gpg -K                               # 你的签名密钥，私钥部分在卡上
```

Windows 上的代理窗口里，每次签名操作都应看到一行 `connect -> ...` 和一行
`closed  up=.. down=..`。

## 在服务器上签名提交

```sh
git config --global gpg.program gpg      # 绝不能是 C:\... 这类 Windows 路径
git config --global gpg.format openpgp
git config --global user.signingkey <KEYID>
git config --global commit.gpgsign true

git commit -S -m "test"
```

PIN 与触摸提示会出现在 **Windows 本机**，因为持有密钥的 agent 运行在那边。
如果提交像是卡住了，请看代理窗口和 YubiKey 的指示灯。

## 故障排查

| 现象 | 原因与处理 |
| --- | --- |
| `cannot listen on 127.0.0.1:16688: ... Only one usage of each socket address ...` | 已经有一个实例在运行。停掉它（在其窗口按 `Ctrl+C`），或改用别的 `-listen` 并同步修改 ssh 配置。 |
| `warning: cannot read that file yet: open ...S.gpg-agent.extra: The system cannot find the file specified` | `gpg-agent` 没有运行，或端点文件在别处。启动 Kleopatra 或执行 `gpg-connect-agent /bye`，再与 `gpgconf --list-dirs agent-extra-socket` 对照。 |
| `remote port forwarding failed for listen path /run/user/1000/gnupg/S.gpg-agent` | 远端 agent 仍占着 socket：执行 `gpgconf --kill gpg-agent`、加上 `no-autostart`、mask 掉 systemd 单元，然后重连。 |
| 远端 `gpg-connect-agent` 报出的是服务器自己的 GnuPG 版本 | 转送未生效：关注上面的 ssh 警告，用 `gpgconf --list-dirs agent-socket` 核对远端路径，并确认 `sshd_config` 里有 `StreamLocalBindUnlink yes`。 |
| Linux 上出现 `gpg: cannot run C:\Program Files\GnuPG\bin\gpg.exe: No such file or directory` | 服务器的 git 里 `gpg.program` 指向了 Windows 路径（常是从 Windows 的 `.gitconfig` 抄来的）。修正：`git config --global gpg.program gpg`。 |
| `gpg: skipped "KEYID": No secret key` | 公钥没有导入服务器，或 `user.signingkey` 指到了别的密钥。见第 2 步。 |
| 签名时代理窗口毫无输出 | ssh 没有在转送：确认本次会话没有转送警告，且代理监听的地址与 `RemoteForward` 右侧一致。 |
| 本来能用，过一会儿就不行了 | 若 `gpg-agent` 被重启过，代理已经自动跟随新的端口与 nonce。请确认 YubiKey 仍插着，以及 PIN 缓存是否过期（`default-cache-ttl`）。 |
| 启动 exe 后 Windows 弹出防火墙提示 | 并不需要放行：监听只绑定在 `127.0.0.1`。可以放心拒绝对公网开放。 |

## 安全须知

- 桥接程序只监听 `127.0.0.1`。不要把 `-listen` 改成 `0.0.0.0`：它所代理的
  agent 端点本就应当留在本机。
- 它会代表本机调用者补送 nonce，这与本机任何进程读取 `S.gpg-agent.extra`
  所获得的能力相同。请把这个端口视为与你的 agent 同等敏感。
- 连接期间，远端主机可以请求你的 agent 签名。这正是本工具的目的，请只对
  你信任的主机使用。
- 私钥永不离开 YubiKey，隧道里只走签名请求与结果。

## 说明与限制

- 按设计只支持 Windows：它解析的是 GnuPG 在 Windows 上的端点文件格式。
- 控制台输出（含报错信息）全部为英文。
- 程序没有任何第三方依赖，`main.go` 就是全部。
- 如果将来某个 GnuPG 版本在 Windows 上提供了真正的 Unix socket，本桥接程序
  就不再有存在的必要，直接写 `RemoteForward <远端 socket> <本地 socket>`
  即可。
