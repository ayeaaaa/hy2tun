package main

import (
	"flag"
	"fmt"
	"os"

	"hy2tun/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("c", "client.yaml", "path to client config")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	fmt.Fprintf(os.Stderr, "hy2tun: client startup is not wired yet (config: %s)\n", *configPath)
	os.Exit(1)
}
