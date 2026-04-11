package replication_test

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/junyoung/core-x/internal/infrastructure/replication"
	"github.com/junyoung/core-x/internal/infrastructure/storage/wal"
	pb "github.com/junyoung/core-x/proto/pb"
)

func TestReplicationClient_ReceivesEntries(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/primary.wal"
	replicaPath := dir + "/replica.wal"

	pw, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	pw.Write([]byte("alpha"))
	pw.Write([]byte("beta"))
	pw.Write([]byte("gamma"))
	pw.Sync()
	pw.Close()

	walFile, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walFile.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(walFile, pw.CompactionNotify, lag, 10*time.Millisecond)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterReplicationServiceServer(gs, replication.NewReplicationServer(streamer))
	go gs.Serve(lis)
	defer gs.Stop()

	receiver, err := replication.NewReceiver(replicaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := replication.NewReplicationClient(
		lis.Addr().String(),
		"replica-test",
		receiver,
		lag,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)

	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	// Wait until replica WAL has 3 records.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		offset, _ := receiver.PersistedOffset()
		if offset > 0 {
			r, err := wal.NewReader(replicaPath)
			if err == nil {
				count := 0
				for r.Scan() {
					count++
				}
				r.Close()
				if count == 3 {
					cancel()
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	<-done

	r, err := wal.NewReader(replicaPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	count := 0
	for r.Scan() {
		count++
	}
	if count != 3 {
		t.Errorf("replica WAL has %d records, want 3", count)
	}
}
