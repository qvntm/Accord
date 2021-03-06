package main

import (
	"log"

	"github.com/qvntm/accord"
)

func main() {
	s := accord.NewAccordServer()
	if _, err := s.Listen("0.0.0.0:50051"); err != nil {
		log.Fatalf("Server failed to listen: %v", err)
	}
	s.Start()
}
