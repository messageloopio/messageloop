package main

import (
	"log"

	"github.com/fleetlit/messageloop/sdks/go/example"
)

func main() {
	if err := example.RPCExample(); err != nil {
		log.Fatal(err)
	}
}
