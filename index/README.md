# 索引

一个 JSON 文件，客户端整份下载后**本地搜索**。里面只存指针，不存实质内容 ——
`source` 那个 git 地址永远是真相。

v1 就用文件：

```bash
toy index add local file:///root/cirraya/Private/Toy-Community/index/toy-index.json
```

以后传到任何静态托管（对象存储、GitHub raw、自己的服务器）只要改一行 URL，
**客户端代码一个字节都不用动** —— 因为它从头到尾只认识"一个 URL"。

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

## 加一条

```bash
cd 你的项目 && toy publish     # 输出一条可以直接粘进来的条目
```

## 删一条会怎样

**只是不再被搜到。** `toy install github:owner/repo` 照样装得上。
任何人 fork 这个文件就是一个完整的平行索引 —— 没有人能真的封杀一个包，包括我们。
