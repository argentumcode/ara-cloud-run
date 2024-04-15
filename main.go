package main

import (
	"github.com/argentumcode/ara-cloud-run/cmd"
)

func main() {
	if err := cmd.NewCmd().Execute(); err != nil {
		panic(err)
	}
}
