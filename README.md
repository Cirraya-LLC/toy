# toy

**装那些没人给你打包的小工具。装的时候一行脚本都不跑。**

```bash
toy install github:Derrity/tinyserve   # 知道仓库在哪，直接装
tinyserve                              # 直接就能用
```

想按名字找工具（`toy search json`、`toy install Derrity/tinyserve`）就先加一个索引 ——
**v1 一个都不预置**，见下面「从哪都能装」。

---

## 为什么

因为把一个小工具送到别人手里，现在的路都太长了。

进 Debian 要等一个 2–3 年的发行周期，而且[给非平凡的东西打包能花掉你五天](https://news.ycombinator.com/item?id=26211842)。
进 Homebrew core 要过审。开个 tap 又等于[让一个陌生人的 Ruby 代码在别人机器上执行](https://docs.brew.sh/Taps) ——
Homebrew 6.0 不得不为此加了一道 Tap Trust。

于是大家退回到 `curl | bash`，把整台机器交给一个没人看过的脚本。

toy 的答案是把这两头都砍掉：**发布只要 8 行 toml，安装的时候什么都不执行。**

## 两条不会变的规矩

**一、装的时候不跑任何代码。**
没有 install、没有 postinstall、没有 build、没有 hook，一个都没有。
toy 只会下载、验哈希、解压、建软链。

这不是省事，是这个项目存在的理由。吃掉 700+ 个 npm 包的 Shai-Hulud 蠕虫，
传播方式就是 `preinstall` 钩子。一个从设计上就没有钩子的分发器，这类攻击根本无从谈起。

**二、包和包之间没有依赖。**
每个 toy 是一个自包含的二进制或一个单文件脚本。没有依赖图，就没有依赖地狱，
也没有 left-pad。

## 发布一个工具

在你的仓库根上放一个 `toy.toml`：

```toml
name    = "tinyserve"
desc    = "一个很小的静态文件服务器"
license = "MIT"

[bin]
tinyserve = "tinyserve"

[artifact]
"linux/amd64"  = "tinyserve_{version}_linux_amd64.tar.gz"
"darwin/arm64" = "tinyserve_{version}_darwin_arm64.tar.gz"
```

打个 tag，发个 release。**完事了。**

没有 URL、没有版本号、没有托管方名字 —— 所以下次发版这个文件一个字都不用改。
不用注册、不用等审核、不用求任何人。

```bash
toy install github:你的名字/你的工具   # 别人现在就能装
```

想被搜到，再把一行条目提进索引。跑一次 `toy keygen` 就能给自己的条目签名。
**这两步都是可选的。**

## 你在信任谁，toy 会告诉你

```
✓ 已签名 · key a3f9… · 索引 community 登记（不代表 github/Derrity 本人）
· 来源 github.com/Derrity · 未签名       你信的是 GitHub 的账号体系
⚠ 未签名 · 首次安装，已钉住 sha256        你什么都没信，但内容变了会拦下来
```

哈希对不上就拒绝安装。公钥或哈希变了会大声警告并停下。模型抄的是 SSH 的 `known_hosts`。

**「✓ 已签名」证明的是连续性，不是身份。** 索引谁都能 fork、谁都能提 PR，所以那把
公钥和 GitHub 账号之间没有任何绑定 —— 一个人完全可以 fork 一份索引、用**自己的**
密钥给同一个 `id` 重新签一条、把 `source` 指向自己的仓库，第一次安装的人照样看到
这个勾。它真正保证的是：**你上次从这个 id 装到的东西，和这次是同一个人签的**（公钥
首次安装即钉住，换了就拒绝安装）。所以那一行永远带着「不代表某某本人」这句尾巴，
第一次安装时该看的是 `source` 指向哪里，不是有没有勾。

## 从哪都能装

```bash
toy install Derrity/tinyserve            # 查索引
toy install github:Derrity/tinyserve     # 跳过索引，直接读仓库
toy install gitlab:x/y                   # 加三行配置就多支持一家托管方
toy install https://…/toy.toml           # 完全裸奔
```

索引里只存一个指向仓库的指针，**真相永远是那个 git 地址**。
所以从索引里删掉一条只是"不再被搜到"，装还是装得上 —— 没有人能真的封杀一个包。

**v1 没有预置任何索引**，因为公共索引还没上线 —— 与其塞一个拉不到的地址让你第一次
`toy doctor` 就看见一条红叉，不如老实说没有。要开搜就自己加一个（索引就是一个 JSON
文件，`https://` 和本地路径一样用）：

```bash
toy index add local ./index/toy-index.json          # 本仓库里那份种子
toy index add community https://example.com/index.json
toy index list                                       # 加了哪些
```

没有索引不影响装东西：`toy install github:owner/repo` 那条路本来就不经过索引。

托管方是一张配置表不是三份代码，版本发现用的是 `git ls-remote`，
所以 toy 不依赖任何一家的 API，也就不会被任何一家的限流卡住。

## 命令

```
toy install [--force] [--version v] <ref>        装一个包
toy uninstall <名字>                              卸掉（缓存留着）
toy update [名字]                                 升级，不带名字就是全部
toy list                                         已经装了什么
toy search [--limit n] [--json] <关键词>           在索引里搜（纯本地，不联网搜）
toy info <ref>                                   装之前先看一眼：平台、命令名、信任状态
toy index add <名字> <地址> | rm <名字> | list       索引管理
toy keygen                                       生成 ed25519 密钥对
toy publish [--source github:owner/repo] [目录]   签名，输出一条能粘进索引的条目
toy serve [--addr host:port]                     把本机缓存变成一个镜像
toy doctor                                       检查 PATH、目录权限、索引可达性
```

**选项要写在包名前面**（`toy install --force github:o/r`）—— Go 的 flag 解析
碰到第一个非选项参数就停了。

`toy serve` 把你本机的缓存变成一个镜像。因为内容是哈希寻址的，
镜像不需要被信任 —— 谁都可以当，五分钟的事。上游哪天没了，包还活着。
**真的走了镜像，安装时会明说是从哪拿的** —— 不需要信任不等于不需要知道。

## 不做的事

不做源码构建、不做包间依赖、不做 P2P/DHT、不做 Windows（暂时）。
每一条都是想清楚之后砍掉的，理由写在 the design notes 里。

## 装 toy 自己

```bash
go install github.com/Cirraya-LLC/toy/cmd/toy@latest
```

装完把 `~/.toy/bin` 加进 PATH，跑 `toy doctor` 检查一遍。
`doctor` 会说"没有配索引" —— 那是默认状态不是故障，装东西不受影响。

## 许可

MIT
