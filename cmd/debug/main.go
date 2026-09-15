package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/paluszkiewiczB/popcorn"
)

func main() {
	const total = 20
	const cap = 10

	bus, err := popcorn.NewBus()
	if err != nil {
		panic(err)
	}

	bus.StartBuffering()

	ch := make(chan popcorn.Event, cap)
	if err := bus.Subscribe("a", ch); err != nil {
		panic(err)
	}

	var wg sync.WaitGroup

	for range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := popcorn.NewEvent[string]("src", "racing")
			_ = bus.Send(context.Background(), e)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		bus.FinishBuffering()
	}()

	wg.Wait()

	count := 0
	for range total {
		select {
		case <-ch:
			count++
		case <-time.After(time.Second):
			fmt.Printf("timeout after reading %d events\n", count)
			os.Exit(1)
		}
	}

	fmt.Printf("success: read %d events\n", count)
}
