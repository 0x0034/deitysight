//go:build integration

// Test-only process/thread population. It does not run inside the agent service.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { runtime.LockOSThread(); wg.Done(); time.Sleep(90 * time.Second) }()
		}
		wg.Wait()
		time.Sleep(90 * time.Second)
		return
	}
	var children []*exec.Cmd
	defer func() {
		for _, c := range children {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}()
	for i := 0; i < 100; i++ {
		c := exec.Command(os.Args[0], "child")
		if e := c.Start(); e != nil {
			panic(e)
		}
		children = append(children, c)
	}
	fmt.Println("100 worker processes started, each with 8 locked application threads plus Go runtime threads")
	time.Sleep(85 * time.Second)
}
