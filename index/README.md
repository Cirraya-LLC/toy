# 索引

一个 JSON 文件，客户端整份下载后**本地搜索**。里面只存指针，不存实质内容 ——
`source` 那个 git 地址永远是真相。

v1 就用文件，而且 **toy 一个索引都不预置** —— 想搜就自己加一条：

```bash
toy index add local ./index/toy-index.json     # 裸路径会自动补成 file:///绝对路径
toy index add local file:///绝对路径/index/toy-index.json   # 写全了也行
```

以后传到任何静态托管（对象存储、GitHub raw、自己的服务器）只要改一行 URL，
**客户端代码一个字节都不用动** —— 因为它从头到尾只认识"一个 URL"。
真要把它变成所有人的默认索引，改的就是 `internal/store/store.go` 里那个
`DefaultIndexURL` 常量，别处一行都不用碰。

条目里的 `source` 只能是托管方形态（`github:owner/repo`）或者 **`https://`** 的裸地址：
索引是远端内容，放行 `file://` 等于让任何一份索引指使客户端去读本机的文件，
放行明文 `http://` 等于把它指向 `169.254.169.254` 这种只在受害者机器上才有意义的地址。
不合规的条目会被静默跳过并计数（`toy search` 会警告有几条被跳过）。

## 条目长什么样

```json
{
  "id":       "Derrity/tinyserve",
  "source":   "github:Derrity/tinyserve",
  "desc":     "一个很小的静态文件服务器",
  "tags":     ["web", "server"],
  "key":      "a3f9…",
  "installs": 0
}
```

`key` 是作者的 ed25519 公钥，跑过 `toy keygen` 认领之后才有。没有它也能被搜到、被装上。

`sig`（`toy publish` 一起生成）是作者对 `id + source + 那一份 toy.toml 原文` 的签名。
**光有 key 不算已签名** —— 这个文件谁都能 fork、谁都能提 PR，抄一把公钥是零成本的。
客户端验过 `sig` 才会打「✓ 已签名」，验不过就降级成「来源 …」并说明原因。

## 加一条

```bash
cd 你的项目 && toy publish     # 输出一条可以直接粘进来的条目
```

## 删一条会怎样

**只是不再被搜到。** `toy install github:owner/repo` 照样装得上。
任何人 fork 这个文件就是一个完整的平行索引 —— 没有人能真的封杀一个包，包括我们。
