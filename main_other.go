//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("SFTP Syncer 当前提供的是 Windows GUI 版本；请在 Windows 上构建和运行。")
}
