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

func TestReplicationServer_StreamWAL(t *testing.T) {
	dir := t.TempDir()
	walPath := dir + "/test.wal"

	w, err := wal.NewWriter(wal.Config{Path: walPath, SyncPolicy: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("entry-A"))
	w.Write([]byte("entry-B"))
	w.Sync()
	w.Close()

	walFile, err := os.Open(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer walFile.Close()

	lag := replication.NewReplicationLag()
	streamer := replication.NewStreamer(walFile, w.CompactionNotify, lag, 10*time.Millisecond)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterReplicationServiceServer(gs, replication.NewReplicationServer(streamer))
	go gs.Serve(lis)
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	client := pb.NewReplicationServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.StreamWAL(ctx, &pb.StreamWALRequest{ReplicaId: "test-replica", StartOffset: 0})
	if err != nil {
		t.Fatal(err)
	}

	var received []*pb.WALEntry
	for {
		entry, err := stream.Recv()
		if err != nil {
			break
		}
		received = append(received, entry)
		if len(received) == 2 {
			cancel()
		}
	}

	if len(received) != 2 {
		t.Errorf("received %d entries, want 2", len(received))
	}
}
