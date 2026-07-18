package main

import (
	"fmt"
	"log"
	"time"

	"homelab-tsdb/internal/storage"
	"homelab-tsdb/internal/wal"
)

func main() {

	const walPath = "data.wal"

	fmt.Println("==== Fresh start...Starting WAL ====")
	runFreshWrite(walPath)

	fmt.Println("==== Simulate crash + restart, replay from WAL ====")
	runRecovery(walPath)
}

func runFreshWrite(walPath string) {
	w, err := wal.Open(walPath)
	if err != nil {
		log.Fatalf("open wal: %v", err)
	}

	mem := storage.NewMemtable()
	now := time.Now().UnixNano()

	points := []storage.Point{
		{Series: "cpu.usage", Tags: map[string]string{"host": "pi4"}, Timestamp: now, Value: 12.5},
		{Series: "cpu.usage", Tags: map[string]string{"host": "pi4"}, Timestamp: now + 1e9, Value: 14.1},
		{Series: "mem.used_mb", Tags: map[string]string{"host": "pi4"}, Timestamp: now, Value: 812},
	}

	for _, p := range points {
		if err := w.Append(p.Encode()); err != nil {
			log.Fatalf("write point to wal: %v", err)
		}
		mem.Put(p)
	}

	fmt.Printf("Wrote %d points across %d series to WAL and memtable.\n", mem.Len(), len(mem.SeriesNames()))
	/*
		Delibaretly NOT calling w.Close() here to simulate a crash before the WAL is closed
		Append() alrady fsynced each write, so nothing after the last succesful Append() should be lost.
	*/
	_ = w
}

func runRecovery(walPath string) {
	mem := storage.NewMemtable()
	recovered := 0

	err := wal.Replay(walPath, func(payload []byte) error {
		p, err := storage.DecodePoint(payload)
		if err != nil {
			return err
		}
		mem.Put(p)
		recovered++
		return nil
	})
	if err != nil {
		log.Fatalf("replay wal: %v", err)
	}

	fmt.Printf("Recovered %d points from WAL after 'crash'.\n", recovered)
	for _, series := range mem.SeriesNames() {
		pts := mem.Range(series, 0, time.Now().Add(24*time.Hour).UnixNano())
		fmt.Printf(" %s: %d points(s)\n", series, len(pts))
		for _, p := range pts {
			fmt.Printf("   t=%d value=%.2f tags=%v\n", p.Timestamp, p.Value, p.Tags)
		}
	}
}
