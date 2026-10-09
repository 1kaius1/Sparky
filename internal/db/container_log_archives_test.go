// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func archiveInput(nodeID, uploadID string) NewContainerLogArchive {
	code := 137
	started := time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC)
	lines := 2000
	return NewContainerLogArchive{
		UploadID: uploadID, NodeID: nodeID, ProfileName: "tiny", ContainerName: "sparky-tiny-20261009-010203", ContainerID: "abc",
		Reason: "unload", State: "exited", ExitCode: &code, OOMKilled: true, StartedAt: &started, LinesRequested: &lines,
		LinesKept: 3, Truncated: true, SizeBytes: 1234, LogGz: []byte{0x1f, 0x8b, 1, 2, 3},
	}
}

func cleanupArchives(t *testing.T, repo *ContainerLogArchiveRepository, nodeID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(context.Background(), `DELETE FROM container_log_archives WHERE node_id = $1`, nodeID)
	})
}

func TestContainerLogArchiveRepository_CreateAndFind(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	repo := NewContainerLogArchiveRepository(pool)
	ctx := context.Background()
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	cleanupArchives(t, repo, node.ID)

	in := archiveInput(node.ID, "upload-"+t.Name())
	created, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if created.ID == "" || created.NodeName != node.Name || created.CompressedBytes != int64(len(in.LogGz)) {
		t.Errorf("created = %+v", created)
	}

	got, body, err := repo.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("FindByID() error: %v", err)
	}
	if string(body) != string(in.LogGz) {
		t.Errorf("body = %v, want %v", body, in.LogGz)
	}
	if got.ExitCode == nil || *got.ExitCode != 137 || !got.OOMKilled || !got.Truncated || got.LinesRequested == nil || *got.LinesRequested != 2000 ||
		got.LinesKept != 3 || got.SizeBytes != 1234 || got.State != "exited" || got.Reason != "unload" || got.FinishedAt != nil || got.StartedAt == nil {
		t.Errorf("got = %+v", got)
	}
}

// An agent that retries an upload whose confirmation it never saw must not
// store the log twice.
func TestContainerLogArchiveRepository_CreateIsIdempotentOnUploadID(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	repo := NewContainerLogArchiveRepository(pool)
	ctx := context.Background()
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	cleanupArchives(t, repo, node.ID)

	in := archiveInput(node.ID, "upload-"+t.Name())
	first, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.LogGz = []byte("different bytes on the retry")
	second, err := repo.Create(ctx, in)
	if err != nil {
		t.Fatalf("second Create() error: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("retry stored a second row: %s vs %s", second.ID, first.ID)
	}
	list, _ := repo.List(ctx, 100)
	n := 0
	for _, a := range list {
		if a.NodeID == node.ID {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d rows for the node, want 1", n)
	}
}

func TestContainerLogArchiveRepository_ListIsNewestFirstAndRespectsTheLimit(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	repo := NewContainerLogArchiveRepository(pool)
	ctx := context.Background()
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	cleanupArchives(t, repo, node.ID)

	var ids []string
	for i := 0; i < 3; i++ {
		a, err := repo.Create(ctx, archiveInput(node.ID, fmt.Sprintf("upload-%s-%d", t.Name(), i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
		time.Sleep(10 * time.Millisecond)
	}
	got, err := repo.List(ctx, 2)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(got) > 2 {
		t.Fatalf("List(2) returned %d rows", len(got))
	}
	if len(got) == 2 && (got[0].ID != ids[2] || got[1].ID != ids[1]) {
		// Other tests' rows may share the table; only assert when ours lead.
		if got[0].NodeID == node.ID && got[1].NodeID == node.ID {
			t.Errorf("order = %s, %s; want newest first (%s, %s)", got[0].ID, got[1].ID, ids[2], ids[1])
		}
	}
}

func TestContainerLogArchiveRepository_FindByIDNotFound(t *testing.T) {
	pool := newTestPool(t)
	repo := NewContainerLogArchiveRepository(pool)
	_, _, err := repo.FindByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != ErrContainerLogArchiveNotFound {
		t.Errorf("err = %v, want ErrContainerLogArchiveNotFound", err)
	}
}

func TestContainerLogArchiveRepository_LatestByInstanceIDs(t *testing.T) {
	pool := newTestPool(t)
	nodes := NewNodeRepository(pool)
	repo := NewContainerLogArchiveRepository(pool)
	ctx := context.Background()
	node := createTestNode(t, nodes, fmt.Sprintf("node-%s", t.Name()))
	cleanupArchives(t, repo, node.ID)

	instA := "aaaaaaaa-0000-0000-0000-00000000000a"
	instB := "bbbbbbbb-0000-0000-0000-00000000000b"
	instNone := "cccccccc-0000-0000-0000-00000000000c"

	oldA := archiveInput(node.ID, "upload-"+t.Name()+"-a1")
	oldA.InstanceID = &instA
	first, err := repo.Create(ctx, oldA)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	newA := archiveInput(node.ID, "upload-"+t.Name()+"-a2")
	newA.InstanceID = &instA
	second, err := repo.Create(ctx, newA)
	if err != nil {
		t.Fatal(err)
	}
	inB := archiveInput(node.ID, "upload-"+t.Name()+"-b1")
	inB.InstanceID = &instB
	b, err := repo.Create(ctx, inB)
	if err != nil {
		t.Fatal(err)
	}

	got, err := repo.LatestByInstanceIDs(ctx, []string{instA, instB, instNone})
	if err != nil {
		t.Fatalf("LatestByInstanceIDs() error: %v", err)
	}
	if got[instA] != second.ID || got[instA] == first.ID {
		t.Errorf("instance A -> %s, want its newest archive %s", got[instA], second.ID)
	}
	if got[instB] != b.ID {
		t.Errorf("instance B -> %s, want %s", got[instB], b.ID)
	}
	if _, ok := got[instNone]; ok || len(got) != 2 {
		t.Errorf("got %v, want only the two instances that have archives", got)
	}

	empty, err := repo.LatestByInstanceIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input: %v, %v", empty, err)
	}
}
