package main

import "context"

// runKeygen 生成密钥对存进 ~/.toy/keys，打印公钥和指纹。已存在时拒绝覆盖。
func runKeygen(ctx context.Context, args []string) error { panic("todo") }

// runPublish 读当前目录的 toy.toml，校验，用本机私钥签名，
// 打印一条可以直接粘进索引 JSON 的条目。
// 不联网、不提交 —— v1 索引是本地文件，粘过去就行。
func runPublish(ctx context.Context, args []string) error { panic("todo") }

// runServe 把 ~/.toy/cache 当静态目录起个 HTTP server（文件名就是 sha256）。
// 因为内容是 hash 寻址的，这个镜像天然不需要被信任。规格 §8。
func runServe(ctx context.Context, args []string) error { panic("todo") }
