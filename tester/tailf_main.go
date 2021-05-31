package main

import (
	"context"
	"github.com/account-login/single_udp_tun/tailf"
	"log"
	"os"
)

func main() {
	log.SetFlags(log.Flags() | log.Lmicroseconds)

	out := make(chan string, 0)

	files := os.Args[1:]
	tails := make([]tailf.TailF, len(files))
	for i, path := range files {
		tails[i].Path = path
		tails[i].Output = out
		tails[i].Ctx = context.TODO()
		go tails[i].Run()
	}

	for line := range out {
		_, _ = os.Stdout.WriteString(line + "\n")
	}
}
