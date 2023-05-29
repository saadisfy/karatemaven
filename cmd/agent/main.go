package main

import (
	"os"

	"github.com/kubeshop/testkube/pkg/executor/agent"
	"github.com/saadisfy/karatemaven/pkg/runner"
)

func main() {
	agent.Run(runner.NewRunner(), os.Args)
}
