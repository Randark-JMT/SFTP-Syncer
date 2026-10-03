package main

// Version 是应用版本号，默认值为 dev。
// 可通过构建参数覆盖：
//
//	go build -ldflags="-X main.Version=v0.7.0"
//
// GitHub Actions 发布工作流会在手动触发时自动注入该值。
var Version = "dev"
