// Command hello is a hello world program to build into an image with runt
// build (see the Dockerfile next to it, and make hello-image). It is built
// as a static binary, with no C libraries, so it runs in an image FROM
// scratch, which has no other files at all.
package main

import (
	"fmt"
	"os"
)

func main() {
	hostname, _ := os.Hostname()
	fmt.Println("Hello, world!")
	fmt.Printf("I'm running in a container called %q, as PID %d.\n", hostname, os.Getpid())
}
